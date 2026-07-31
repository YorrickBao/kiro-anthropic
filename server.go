package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// Server wires the Anthropic-facing HTTP API to the Kiro backend. All requests
// and control-plane calls are served from the multi-account store; there is no
// single primary account.
type Server struct {
	cfg      *Config
	kiro     *KiroClient
	accounts *AccountStore    // multi-account credential store (the only account source)
	login    *loginManager    // IdC sign-in flow driver
	selector *accountSelector // round-robin picker over the account store
	logger   *slog.Logger     // per-request access log; nil disables it

	modelsMu    sync.Mutex
	modelsCache map[string]modelsCacheEntry // per-account model list cache

	usageMu    sync.Mutex
	usageCache map[string]usageCacheEntry // per-account usage cache

	updateMu    sync.Mutex
	updateCache *updateStatus // last successful GitHub update check (nil until first fetch)
	updateAt    time.Time     // when updateCache was fetched
	updateErr   error         // last failed GitHub check (e.g. rate limit), negatively cached
	updateErrAt time.Time     // when updateErr was recorded
}

// modelsCacheEntry caches one account's model list.
type modelsCacheEntry struct {
	models  []kiroModelInfo
	fetched time.Time
}

// usageCacheEntry caches one account's usage.
type usageCacheEntry struct {
	usage   *kiroUsage
	fetched time.Time
}

// usageCacheTTL is how long a GetUsageLimits result is reused before refetching,
// so the auto-refreshing admin page does not hammer the control plane.
const usageCacheTTL = 60 * time.Second

// modelsCacheTTL is how long a per-account model list is reused.
const modelsCacheTTL = 5 * time.Minute

// aggregateConcurrency caps how many accounts a per-model aggregation fans out
// to in parallel, so a large pool does not stampede the control plane at once.
const aggregateConcurrency = 8

// aggregateTimeout bounds the whole per-model aggregation so one slow or stuck
// account cannot hang the admin query (every per-account call is cache-backed).
const aggregateTimeout = 30 * time.Second

// updateCacheTTL is how long a GitHub release check is reused. It is long
// because the admin page polls periodically and GitHub's unauthenticated rate
// limit is only 60 requests/hour per IP.
const updateCacheTTL = 30 * time.Minute

// updateErrCacheTTL is how long a failed GitHub check is remembered before
// retrying. Negatively caching failures matters most for the rate limit: once
// the unauthenticated 60 req/hour/IP cap is hit, retrying on every admin poll
// only keeps the limit pinned, so we back off and let it reset.
const updateErrCacheTTL = 10 * time.Minute

// updateStatus is the version-update view surfaced to the admin page: the
// running version, the latest release tag, and the notes of every release newer
// than the running version (newest first).
type updateStatus struct {
	Current         string        `json:"current"`
	Latest          string        `json:"latest"`
	UpdateAvailable bool          `json:"update_available"`
	Releases        []releaseNote `json:"releases"`
}

// releaseNote is one GitHub release rendered for the admin page.
type releaseNote struct {
	Tag         string `json:"tag"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
}

// fallbackModels is used for /v1/models when no account can list live models.
var fallbackModels = []string{
	"auto", "claude-sonnet-5", "claude-opus-4.8", "claude-opus-4.7", "claude-opus-4.6",
	"claude-sonnet-4.6", "claude-opus-4.5", "claude-sonnet-4.5", "claude-sonnet-4", "claude-haiku-4.5",
}

func NewServer(cfg *Config, client *http.Client) *Server {
	return &Server{
		cfg:         cfg,
		kiro:        NewKiroClient(cfg, client),
		login:       newLoginManager(client),
		modelsCache: map[string]modelsCacheEntry{},
		usageCache:  map[string]usageCacheEntry{},
	}
}

// setAccounts attaches the multi-account store and builds the round-robin
// selector used to dispatch requests and control-plane calls across accounts.
func (s *Server) setAccounts(accounts *AccountStore, client *http.Client) {
	s.accounts = accounts
	s.selector = newAccountSelector(accounts, client)
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	if s.logger != nil {
		r.Use(s.accessLog)
	}
	// Handle (not Get/Post) preserves the previous ServeMux behaviour of
	// accepting any method; handleMessages does its own method check so it can
	// return an Anthropic-shaped 405.
	r.Handle("/health", http.HandlerFunc(s.handleHealth))
	r.Handle("/v1/models", http.HandlerFunc(s.handleModels))
	r.Handle("/v1/messages", http.HandlerFunc(s.handleMessages))
	r.Post("/api/models/aggregate", s.handleAPIModelAggregate)
	return r
}

// ---- request logging ----

// ctxKeyAccess is the context key under which per-request log details live.
type ctxKeyAccess struct{}

// accessInfo carries request details that handlers attach for the access log.
type accessInfo struct {
	model    string
	stream   bool
	errMsg   string // failure cause, recorded when the request errors
	canceled bool   // client went away mid-stream (not a server failure)
}

func accessFrom(ctx context.Context) *accessInfo {
	a, _ := ctx.Value(ctxKeyAccess{}).(*accessInfo)
	return a
}

// noteError records a failure cause on the request's accessInfo so the access
// log can surface it and raise the level. No-op when logging is disabled (no
// accessInfo in context).
func noteError(ctx context.Context, msg string) {
	if a := accessFrom(ctx); a != nil {
		a.errMsg = msg
	}
}

// noteCanceled records that a stream ended because the client went away. This
// is a normal outcome, not a server fault, so the access log keeps it at Info
// level and reports it under a "canceled" attribute instead of "error".
func noteCanceled(ctx context.Context) {
	if a := accessFrom(ctx); a != nil {
		a.canceled = true
	}
}

// accessLog is a chi middleware that emits one structured (slog) access-log
// line per request. chi's WrapResponseWriter captures the status and byte count
// while preserving the http.Flusher behaviour that streaming (SSE) depends on.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &accessInfo{}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyAccess{}, info))
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // body written without an explicit WriteHeader
		}
		attrs := []any{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start).Round(time.Millisecond)),
			slog.Int("bytes", ww.BytesWritten()),
		}
		if info.model != "" {
			attrs = append(attrs, slog.String("model", info.model))
			mode := "sync"
			if info.stream {
				mode = "stream"
			}
			attrs = append(attrs, slog.String("mode", mode))
		}

		// Level tracks the outcome: 5xx -> Error, 4xx (or a recorded failure
		// cause, e.g. a mid-stream error after the 200 header) -> Warn, else Info.
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400 || info.errMsg != "":
			level = slog.LevelWarn
		}
		if info.errMsg != "" {
			attrs = append(attrs, slog.String("error", info.errMsg))
		}
		// A client disconnect mid-stream is expected, not a failure: keep it at
		// Info and label it separately so it does not pollute the error logs.
		if info.canceled {
			attrs = append(attrs, slog.Bool("canceled", true))
		}
		s.logger.Log(r.Context(), level, "request", attrs...)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "kiro-anthropic",
		"version": version,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeAnthropicError(w, r, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)

	data := make([]map[string]any, 0)
	if models := s.anyModels(r.Context()); len(models) > 0 {
		for _, m := range models {
			data = append(data, modelInfoJSON(m, now))
		}
	} else {
		// Fallback when no account can list models (empty pool or control plane
		// unreachable): ids only.
		for _, id := range fallbackModels {
			data = append(data, map[string]any{
				"type": "model", "id": id, "display_name": id, "created_at": now,
			})
		}
	}

	resp := map[string]any{"data": data, "has_more": false}
	if len(data) > 0 {
		resp["first_id"] = data[0]["id"]
		resp["last_id"] = data[len(data)-1]["id"]
	}
	writeJSON(w, http.StatusOK, resp)
}

// modelInfoJSON renders a Kiro model as an Anthropic ModelInfo object, exposing
// the context window (max_input_tokens), output ceiling (max_tokens) and
// reasoning-effort support (capabilities.effort).
func modelInfoJSON(m kiroModelInfo, now string) map[string]any {
	name := m.ModelName
	if name == "" {
		name = m.ModelID
	}
	info := map[string]any{
		"type":         "model",
		"id":           m.ModelID,
		"display_name": name,
		"created_at":   now,
	}
	if m.TokenLimits.MaxInputTokens > 0 {
		info["max_input_tokens"] = m.TokenLimits.MaxInputTokens
	}
	if m.TokenLimits.MaxOutputTokens > 0 {
		info["max_tokens"] = m.TokenLimits.MaxOutputTokens
	}
	if m.RateMultiplier > 0 {
		info["rate_multiplier"] = m.RateMultiplier
		if m.RateUnit != "" {
			info["rate_unit"] = m.RateUnit
		}
	}
	if m.Description != "" {
		info["description"] = m.Description
	}
	if m.Status != "" {
		info["status"] = m.Status
	}
	if len(m.SupportedInputTypes) > 0 {
		info["supported_input_types"] = m.SupportedInputTypes
	}
	if m.PromptCaching != nil {
		info["prompt_caching"] = map[string]any{
			"supported": m.PromptCaching.SupportsPromptCaching,
		}
	}

	effortCap := map[string]any{"supported": false}
	for _, lvl := range allEffortLevels {
		effortCap[lvl] = false
	}
	if ec, ok := m.effort(); ok {
		effortCap["supported"] = true
		for _, lvl := range allEffortLevels {
			effortCap[lvl] = ec.has(lvl)
		}
	}
	info["capabilities"] = map[string]any{"effort": effortCap}
	return info
}

// ensureModels fetches one account's model list, caching it per-account for
// modelsCacheTTL. A failed fetch is not cached (it retries next call) and its
// error is returned so per-account callers (modelsByAccount) can surface the
// real upstream cause; "any models" callers (anyModels) ignore it.
func (s *Server) ensureModels(ctx context.Context, creds *accountCreds) ([]kiroModelInfo, error) {
	s.modelsMu.Lock()
	if e, ok := s.modelsCache[creds.id]; ok && time.Since(e.fetched) < modelsCacheTTL {
		s.modelsMu.Unlock()
		return e.models, nil
	}
	s.modelsMu.Unlock()

	models, err := s.kiro.ListModels(ctx, creds)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("upstream returned no models")
	}
	s.modelsMu.Lock()
	s.modelsCache[creds.id] = modelsCacheEntry{models: models, fetched: time.Now()}
	s.modelsMu.Unlock()
	return models, nil
}

// anyModels returns the model list from any one account (model schemas are the
// same across accounts on the same backend). Returns nil when the pool is empty
// or no account can list models; the fetch error is deliberately ignored here
// since the caller (the global model list / /v1/models fallback) degrades to the
// static fallbackModels rather than surfacing per-account failures.
func (s *Server) anyModels(ctx context.Context) []kiroModelInfo {
	if s.selector == nil {
		return nil
	}
	creds, ok := s.selector.peekAny()
	if !ok {
		return nil
	}
	models, _ := s.ensureModels(ctx, creds)
	return models
}

// modelsByAccount fetches the model list using one specific account's
// credentials, bypassing the pool's "pick any" logic so the result reflects that
// account's own tier/entitlement. It refreshes the token and resolves the
// profileArn lazily via ensureModels/ListModels, so it works even for an account
// that the pool would otherwise skip (disabled, cooling down, missing profile).
// Unlike anyModels, the upstream error is surfaced so the admin can see why a
// given account lists no models (expired token, 403, region mismatch, ...).
func (s *Server) modelsByAccount(ctx context.Context, id string) ([]kiroModelInfo, error) {
	if s.selector == nil {
		return nil, fmt.Errorf("account store is not configured")
	}
	creds, ok := s.selector.byID(id)
	if !ok {
		return nil, fmt.Errorf("account not found: %s", id)
	}
	models, err := s.ensureModels(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("list models for account %s: %w", id, err)
	}
	return models, nil
}

// ensureUsage returns one account's usage, fetching it at most once per
// usageCacheTTL. A failed fetch is not cached and is surfaced to the caller.
func (s *Server) ensureUsage(ctx context.Context, creds *accountCreds) (*kiroUsage, error) {
	s.usageMu.Lock()
	if e, ok := s.usageCache[creds.id]; ok && time.Since(e.fetched) < usageCacheTTL {
		u := e.usage
		s.usageMu.Unlock()
		return u, nil
	}
	s.usageMu.Unlock()

	u, err := s.kiro.GetUsage(ctx, creds)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s.usageMu.Lock()
	s.usageCache[creds.id] = usageCacheEntry{usage: u, fetched: now}
	s.usageMu.Unlock()
	// Reconcile the depleted state with the fresh usage so admin-page refreshes
	// (including manual ones) recover or precisely park depleted accounts.
	if s.selector != nil {
		s.selector.applyUsage(creds.id, u, now, false)
	}
	return u, nil
}

// ensureUsageReadOnly returns one account's usage without reconciling the
// selector's depleted state — the read-only path for queries (e.g. the
// per-model aggregate) that must not park accounts as a side effect of
// inspection. It shares ensureUsage's cache; a miss fetches once and fills the
// cache but skips applyUsage, so inspection leaves routing untouched.
func (s *Server) ensureUsageReadOnly(ctx context.Context, creds *accountCreds) (*kiroUsage, error) {
	s.usageMu.Lock()
	if e, ok := s.usageCache[creds.id]; ok && time.Since(e.fetched) < usageCacheTTL {
		u := e.usage
		s.usageMu.Unlock()
		return u, nil
	}
	s.usageMu.Unlock()

	u, err := s.kiro.GetUsage(ctx, creds)
	if err != nil {
		return nil, err
	}
	s.usageMu.Lock()
	s.usageCache[creds.id] = usageCacheEntry{usage: u, fetched: time.Now()}
	s.usageMu.Unlock()
	return u, nil
}

// warmAccount non-blockingly pre-fetches models and usage for one account so the
// first real request to that account does not pay the fetch latency. Best-effort:
// failures are silently ignored (caches stay cold, the request path fetches).
func (s *Server) warmAccount(id string) {
	if s.selector == nil || s.kiro == nil {
		return
	}
	creds, ok := s.selector.byID(id)
	if !ok {
		return
	}
	go func() { s.ensureModels(context.Background(), creds) }()
	go func() { s.ensureUsage(context.Background(), creds) }()
}

// warmAllAccounts non-blockingly pre-fetches models and usage for every usable
// account so the first request to a cold pool does not pay the fetch latency.
func (s *Server) warmAllAccounts() {
	if s.accounts == nil {
		return
	}
	for _, a := range s.accounts.List() {
		if accountUsable(a) {
			s.warmAccount(a.ID)
		}
	}
}

// aggregateModelUsage sums the remaining credit across every account in the
// pool that is currently usable AND serves the given model. It answers "how
// much headroom does this model still have across all accounts" for pools
// whose accounts differ by region/tier entitlement.
//
// An account is excluded when it is disabled or credentialless (accountUsable),
// when its model-list or usage fetch fails (token expired, 403, transport —
// recorded in Errors), when it does not serve the model, or when its base
// credit is exhausted with no overage fallback (Credit nil, or Remaining <= 0
// unless the account opts in via OverageEnabled, upstream has overage active
// (overageActive), it still has overage budget, and the selector has not parked
// it after a real depletion response). An account serving on overage alone
// (base spent, overage active, opt-in, budget left) is INCLUDED and flagged
// OnOverage, so this view matches what applyUsage and the selector's depleted
// state actually route. It is a
// READ-ONLY query: usage is read via ensureUsageReadOnly so it does not park
// accounts in the selector's depleted map (unlike the admin status page, which
// intentionally reconciles). Each per-account call is cache-backed (ensureModels
// 5m / usage 60s); accounts are fanned out concurrently under a cap. The
// "serves this model" test reuses resolveModel (the router's definition) so
// routing and this view agree.
//
// Overage is surfaced as raw upstream fields only (cap/rate/status) plus the
// local opt-in flag and the OnOverage state; no overage "remaining" is derived,
// since getUsageLimits never reports overage used or remaining.
func (s *Server) aggregateModelUsage(ctx context.Context, modelID string) *modelAggregate {
	modelID = strings.TrimSpace(modelID)
	agg := &modelAggregate{Model: modelID, Accounts: []modelAggregateAccount{}}
	if s.selector == nil {
		return agg
	}
	all := s.selector.listAll()

	ctx, cancel := context.WithTimeout(ctx, aggregateTimeout)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)

	var mu sync.Mutex
	sem := make(chan struct{}, aggregateConcurrency)
	for _, c := range all {
		c := c
		if !accountUsable(c.acct) {
			continue
		}
		g.Go(func() error {
			select {
			case sem <- struct{}{}:
			case <-gctx.Done():
				return nil
			}
			defer func() { <-sem }()

			recordErr := func(err error) {
				noteError(gctx, fmt.Sprintf("aggregate %s: account %s: %s", modelID, c.id, err))
				mu.Lock()
				agg.Errors = append(agg.Errors, modelAggregateError{ID: c.id, Error: err.Error()})
				mu.Unlock()
			}

			// Fetch the model list first so a non-serving account short-circuits
			// before the usage fetch — avoids a wasted control-plane round trip.
			models, err := s.ensureModels(gctx, c)
			if err != nil {
				recordErr(err)
				return nil
			}
			if _, _, ok := resolveModel(modelID, models); !ok {
				return nil
			}
			u, err := s.ensureUsageReadOnly(gctx, c)
			if err != nil {
				recordErr(err)
				return nil
			}
			if u.Credit == nil {
				return nil
			}
			// Base exhausted: include only when the account opts in via
			// OverageEnabled, still has overage budget, and has not been parked
			// after a real depletion response. The last check is essential when
			// upstream clamps currentUsage at the base limit: overageRemaining is
			// then optimistic, while the selector's reactive mark is authoritative.
			// Flag included accounts so the UI can identify overage-only service.
			onOverage := false
			if u.Credit.Remaining <= 0 {
				if !c.acct.OverageEnabled || !overageActive(u) || overageRemaining(u.Credit) <= 0 || s.selector.isDepleted(c.id) {
					return nil
				}
				onOverage = true
			}
			mu.Lock()
			agg.Accounts = append(agg.Accounts, modelAggregateAccount{
				ID:            c.id,
				Label:         c.acct.Label,
				Region:        c.acct.Region,
				Limit:         u.Credit.Limit,
				Used:          u.Credit.Used,
				Remaining:     u.Credit.Remaining,
				ResetAt:       u.ResetAt,
				OverageCap:    u.Credit.OverageCap,
				OverageRate:   u.Credit.OverageRate,
				OverageStatus: u.OverageStatus,
				OverageOn:     c.acct.OverageEnabled,
				OnOverage:     onOverage,
			})
			agg.Totals.Accounts++
			agg.Totals.Limit += u.Credit.Limit
			agg.Totals.Used += u.Credit.Used
			agg.Totals.Remaining += u.Credit.Remaining
			if c.acct.OverageEnabled && overageActive(u) && u.Credit.OverageCap > 0 {
				agg.Totals.OverageCap += u.Credit.OverageCap
				agg.Totals.OverageAccts++
			}
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	// excluded is derivable: every pooled account is either summed into
	// Accounts or excluded (unusable / not serving / exhausted / failed / not
	// reached before the deadline). Computing it post-hoc avoids a shared
	// counter and the data race a concurrent counter caused.
	agg.Excluded = len(all) - len(agg.Accounts)
	// Signal an incomplete fan-out so callers don't mistake a deadline-cut
	// result for the full pool.
	if ctx.Err() != nil {
		agg.Truncated = true
	}
	return agg
}

// modelAggregate is the per-model credit summary returned by the aggregate
// admin endpoint: summed totals across usable accounts that serve the model,
// plus a per-account breakdown for debugging.
type modelAggregate struct {
	Model     string                  `json:"model"`
	Totals    modelAggregateTotals    `json:"totals"`
	Accounts  []modelAggregateAccount `json:"accounts"`
	Excluded  int                     `json:"excluded"`
	Truncated bool                    `json:"truncated,omitempty"`
	Errors    []modelAggregateError   `json:"errors,omitempty"`
}

type modelAggregateTotals struct {
	Accounts     int     `json:"accounts"`
	Limit        float64 `json:"limit"`
	Used         float64 `json:"used"`
	Remaining    float64 `json:"remaining"`
	OverageCap   float64 `json:"overage_cap,omitempty"`      // opt-in accounts' overage cap sum (raw field sum, not remaining)
	OverageAccts int     `json:"overage_accounts,omitempty"` // how many of them opted into overage
}

type modelAggregateAccount struct {
	ID            string  `json:"id"`
	Label         string  `json:"label,omitempty"`
	Region        string  `json:"region,omitempty"`
	Limit         float64 `json:"limit"`
	Used          float64 `json:"used"`
	Remaining     float64 `json:"remaining"`
	ResetAt       string  `json:"reset_at,omitempty"`
	OverageCap    float64 `json:"overage_cap,omitempty"`     // raw upstream field
	OverageRate   float64 `json:"overage_rate,omitempty"`    // raw upstream field
	OverageStatus string  `json:"overage_status,omitempty"`  // raw upstream enum
	OverageOn     bool    `json:"overage_enabled,omitempty"` // local opt-in toggle
	OnOverage     bool    `json:"on_overage,omitempty"`      // base spent, serving on overage (matches applyUsage)
}

type modelAggregateError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// ensureUpdateStatus returns the version-update view, querying GitHub at most
// once per updateCacheTTL. It is two-stage to spare the tight unauthenticated
// REST rate limit: a cheap github.com redirect probe resolves the latest tag
// (not rate-limited), and only when that tag is newer than the running build do
// we spend one api.github.com call to aggregate the release notes. Both calls
// reuse the proxy-aware shared client (s.kiro.client). A failed fetch is
// negatively cached for updateErrCacheTTL and surfaced to the caller (the admin
// handler degrades softly), so we back off rather than hammering GitHub.
func (s *Server) ensureUpdateStatus(ctx context.Context) (*updateStatus, error) {
	s.updateMu.Lock()
	if s.updateCache != nil && time.Since(s.updateAt) < updateCacheTTL {
		u := s.updateCache
		s.updateMu.Unlock()
		return u, nil
	}
	// Back off on a recent failure (e.g. GitHub rate limit) instead of retrying
	// on every poll, which would keep the limit pinned and never let it reset.
	if s.updateErr != nil && time.Since(s.updateErrAt) < updateErrCacheTTL {
		err := s.updateErr
		s.updateMu.Unlock()
		return nil, err
	}
	s.updateMu.Unlock()

	fail := func(err error) (*updateStatus, error) {
		s.updateMu.Lock()
		s.updateErr = err
		s.updateErrAt = time.Now()
		s.updateMu.Unlock()
		return nil, err
	}

	// Stage 1: cheap probe. Resolve the latest tag via the github.com redirect,
	// which does not consume the api.github.com rate limit. When we are already
	// up to date (the common case) we stop here and never touch the REST API.
	tag, err := latestTagViaRedirect(ctx, s.kiro.client)
	if err != nil {
		return fail(err)
	}
	st := &updateStatus{Current: version, Releases: []releaseNote{}}
	if !tagIsNewer(version, tag) {
		s.updateMu.Lock()
		s.updateCache = st
		s.updateAt = time.Now()
		s.updateErr = nil
		s.updateErrAt = time.Time{}
		s.updateMu.Unlock()
		return st, nil
	}

	// Stage 2: a newer tag exists, so spend one REST call to aggregate the notes
	// of every release newer than the running version.
	rels, err := listReleases(ctx, s.kiro.client)
	if err != nil {
		return fail(err)
	}
	newer := newerReleases(rels, version)
	if len(newer) > 0 {
		st.UpdateAvailable = true
		st.Latest = newer[0].TagName
		for _, r := range newer {
			st.Releases = append(st.Releases, releaseNote{
				Tag:         r.TagName,
				Name:        r.Name,
				Body:        r.Body,
				HTMLURL:     r.HTMLURL,
				PublishedAt: r.PublishedAt,
			})
		}
	}

	s.updateMu.Lock()
	s.updateCache = st
	s.updateAt = time.Now()
	s.updateErr = nil
	s.updateErrAt = time.Time{}
	s.updateMu.Unlock()
	return st, nil
}

// invalidateUsage drops the cached usage for one account so the next status
// poll refetches it (e.g. after a manual token refresh).
func (s *Server) invalidateUsage(id string) {
	s.usageMu.Lock()
	delete(s.usageCache, id)
	s.usageMu.Unlock()
}

// invalidateModels drops the cached model list for one account, forcing a refetch
// on the next encounter. Used after an INVALID_MODEL_ID rejection, which means
// the cached list no longer matches what the runtime will accept.
func (s *Server) invalidateModels(id string) {
	s.modelsMu.Lock()
	delete(s.modelsCache, id)
	s.modelsMu.Unlock()
}

// modelInfo returns the cached info for a model id from any account. Used to
// clamp effort/max_tokens on outgoing requests.
func (s *Server) modelInfo(ctx context.Context, modelID string) (kiroModelInfo, bool) {
	for _, m := range s.anyModels(ctx) {
		if m.ModelID == modelID {
			return m, true
		}
	}
	return kiroModelInfo{}, false
}

// applyModelRequestFields sets reasoning effort and max output tokens on the
// outgoing request using the model schema resolved from the current message's
// modelId (via the global "any account" list). openStream instead calls
// applyFieldsForModel directly with the per-account matched model.
func (s *Server) applyModelRequestFields(ctx context.Context, kreq *kiroRequest, reqEffort string, reqMaxTokens int) {
	um := kreq.ConversationState.CurrentMessage.UserInputMessage
	if um == nil || um.ModelID == "" || um.ModelID == "auto" {
		return
	}
	model, ok := s.modelInfo(ctx, um.ModelID)
	if !ok {
		return
	}
	applyFieldsForModel(kreq, model, reqEffort, reqMaxTokens)
}

// applyFieldsForModel clamps reasoning effort and max output tokens onto
// kreq.AdditionalModelRequestFields using the matched model's schema directly.
// Both are request-driven with a "top out" default: a client-specified value is
// honored (clamped to the model), otherwise the model's maximum is used. Only
// fields the model's schema actually declares are set, so models without them
// (e.g. "auto", claude-sonnet-4.5) are left untouched and never rejected.
func applyFieldsForModel(kreq *kiroRequest, model kiroModelInfo, reqEffort string, reqMaxTokens int) {
	fields := map[string]any{}

	// Reasoning effort: client's requested level, defaulting to the model's
	// maximum when unspecified. A disabled thinking toggle minimizes it.
	// Always clamped to the advertised levels.
	if ec, ok := model.effort(); ok {
		var level string
		switch desired := strings.TrimSpace(reqEffort); desired {
		case "":
			level = ec.max() // default: top out
		case effortMinimize:
			level = ec.min() // thinking disabled: use the lowest level
		default:
			level = ec.clamp(strings.ToLower(desired))
		}
		if level != "" {
			fields[ec.SchemaPath] = map[string]any{"effort": level}
		}
	}

	// Max output tokens: client's max_tokens, defaulting to the model's ceiling
	// when unspecified. Always clamped into the model's [min, max] range so a
	// small caller value (e.g. 80) doesn't fall under the schema minimum.
	if lo, hi, ok := model.maxTokensRange(); ok {
		want := hi
		if reqMaxTokens > 0 {
			want = reqMaxTokens
			if want > hi {
				want = hi
			}
			if want < lo {
				want = lo
			}
		}
		fields["max_tokens"] = want
	}

	if len(fields) > 0 {
		kreq.AdditionalModelRequestFields = fields
	}
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, r, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeAnthropicError(w, r, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		writeAnthropicError(w, r, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var areq anthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	if info := accessFrom(r.Context()); info != nil {
		info.model = areq.Model
		info.stream = areq.Stream
	}

	// Inline any remote (http/https) image URLs as base64 before translation:
	// Kiro only accepts inline image bytes. Failures degrade gracefully (the
	// block is left as-is and skipped downstream) rather than aborting.
	// Prepare images in one pass: inline remote URLs in the current turn (Kiro
	// only accepts inline bytes) and drop history images — including those
	// nested in tool_result blocks — to a placeholder. Stale history base64
	// would otherwise inflate the raw-byte token estimate returned to callers.
	processImages(r.Context(), &areq, newImageFetcher(s.kiro.client))

	kreq, err := buildKiroRequest(s.cfg, &areq)
	if err != nil {
		writeAnthropicError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Effort, max output tokens, and the modelId itself are resolved per account
	// inside openStream — the chosen account's model list determines both the id
	// and its schema — so nothing model-specific is applied to this seed kreq.

	// Open the upstream stream, dispatching across accounts with pre-stream
	// failover. Once streaming begins (headers sent) no further retry is possible.
	stream, err := s.openStream(r.Context(), kreq, &areq)
	if err != nil {
		if errors.Is(err, errNoAccount) {
			writeAnthropicError(w, r, http.StatusServiceUnavailable, "api_error", err.Error())
			return
		}
		if errors.Is(err, errModelUnavailable) {
			writeAnthropicError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		status, errType := mapUpstreamError(err)
		writeAnthropicError(w, r, status, errType, err.Error())
		return
	}
	defer stream.Close()

	inputChars := len(extractText(areq.System))
	for _, m := range areq.Messages {
		inputChars += len(m.Content)
	}

	if areq.Stream {
		s.streamMessages(w, r, &areq, stream, inputChars)
		return
	}
	s.aggregateMessages(w, r, &areq, stream, inputChars)
}

// maxAccountAttempts caps how many accounts a single request will try before
// giving up, bounding worst-case latency when many accounts are unhealthy.
const maxAccountAttempts = 8

// maxPromptTooLongRetries caps request-level recovery. The initial send is not
// counted, so a request can make at most six context-size attempts.
const maxPromptTooLongRetries = 5

// errNoAccount is returned when the account pool is empty. It maps to a 503 so
// the caller knows to sign in or import one via the admin page.
var errNoAccount = fmt.Errorf("no account available; sign in or import one via the admin page")

// errModelUnavailable is returned when every usable account's model list was
// fetched and none serves the requested model. Mapped to 400 so the client
// knows the model is the problem, not account availability.
var errModelUnavailable = fmt.Errorf("requested model is not available on any account")

// openStream opens the upstream stream, dispatching across stored accounts with
// pre-stream failover. For each account it resolves the requested model against
// that account's available-model list and rebuilds the request with a modelId
// the runtime will accept; accounts that don't serve the model are skipped
// without sending. The per-account profileArn is set on kreq before each
// attempt. An empty pool returns errNoAccount (mapped to 503); a model no
// account serves returns errModelUnavailable (mapped to 400).
//
// A prompt-too-long rejection removes one oldest closed conversation unit and
// retries on the same account, up to maxPromptTooLongRetries. An INVALID_MODEL_ID
// rejection invalidates this account's cached model list and fails over. Other
// request-level errors surface at once; account failures continue through the pool.
func (s *Server) openStream(ctx context.Context, kreq *kiroRequest, areq *anthropicRequest) (*kiroStream, error) {
	if s.selector == nil || len(s.accounts.List()) == 0 {
		return nil, errNoAccount
	}

	// Request-driven model fields are resolved per account (the chosen account's
	// matched model carries its own effort/token schema), so pull them from areq
	// up front.
	reqEffort := ""
	var reqMaxTokens int
	if areq != nil {
		reqEffort = requestedEffort(areq)
		reqMaxTokens = areq.MaxTokens
	}

	tried := map[string]bool{}
	var lastErr error
	promptTooLongRetries := 0
	reasoningStripped := false
	modelSkipped := false // an account was skipped because it doesn't serve the model

	// Per-account resolved model for the current attempt, kept in outer scope so
	// the prompt-too-long rebuild reuses it instead of re-resolving.
	var (
		resolved string
		info     kiroModelInfo
		hasInfo  bool
	)

	// rebuild re-derives the request for the current account: a concrete modelId
	// valid on this account, the per-model request fields, the profileArn, and —
	// if a prior turn stripped reasoning — a stripped history. It replaces kreq.
	rebuild := func(arn string) error {
		next, err := buildKiroRequestWithModel(s.cfg, areq, resolved)
		if err != nil {
			return err
		}
		next.ProfileArn = arn
		if hasInfo {
			applyFieldsForModel(next, info, reqEffort, reqMaxTokens)
		}
		if reasoningStripped {
			stripReasoningFromHistory(next)
		}
		kreq = next
		return nil
	}

	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		creds, ok := s.selector.pick(tried)
		if !ok {
			break
		}
		tried[creds.id] = true

		arn, err := creds.profileArn(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			s.selector.recordFailure(creds.id)
			continue
		}

		// Resolve the model against THIS account's available list and rebuild the
		// request with a modelId the runtime will accept. Accounts that don't
		// serve the model are skipped without sending.
		if areq != nil {
			models, merr := s.ensureModels(ctx, creds)
			switch {
			case merr != nil || len(models) == 0:
				// Can't determine this account's list (cold/network): fall back to
				// the static id guess, and — when the global model cache knows its
				// schema — still apply effort/max_tokens so a transient fetch
				// failure can't silently drop them (the pre-routing behavior).
				resolved = mapModel(areq.Model)
				if mi, ok := s.modelInfo(ctx, resolved); ok {
					info, hasInfo = mi, true
				} else {
					info, hasInfo = kiroModelInfo{}, false
				}
			default:
				r, mi, ok := resolveModel(areq.Model, models)
				if !ok {
					modelSkipped = true
					continue // account doesn't serve this model; try another
				}
				resolved, info, hasInfo = r, mi, true
			}
			if buildErr := rebuild(arn); buildErr != nil {
				return nil, fmt.Errorf("rebuild request for account %s: %w", creds.id, buildErr)
			}
		} else {
			kreq.ProfileArn = arn // nil-areq (test) path: use the request as-is
		}

		for {
			hadReasoning := hasReasoningInHistory(kreq)
			stream, sendErr := s.sendWithReasoningRetry(ctx, creds, kreq)
			if hadReasoning && !hasReasoningInHistory(kreq) {
				reasoningStripped = true
			}

			if sendErr == nil {
				// Peek the first frame before committing. Some upstream failures
				// arrive after a 200 as the very first event, while successful peeks
				// are replayed by the first Recv call.
				perr := firstFrameFailure(stream)
				if perr == nil {
					s.selector.recordSuccess(creds.id)
					return stream, nil
				}
				stream.Close()
				sendErr = perr
				if ctx.Err() != nil {
					// Client went away while peeking: stop, don't burn accounts.
					return nil, perr
				}
			}
			lastErr = sendErr
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			if isPromptTooLongError(sendErr) {
				if areq == nil || promptTooLongRetries >= maxPromptTooLongRetries || !trimOldestCompleteTurn(areq) {
					return nil, sendErr
				}
				if buildErr := rebuild(arn); buildErr != nil {
					return nil, fmt.Errorf("rebuild trimmed request: %w", buildErr)
				}
				promptTooLongRetries++
				continue
			}

			if isThinkingSignatureError(sendErr) && !reasoningStripped && stripReasoningFromHistory(kreq) {
				reasoningStripped = true
				continue
			}

			if isInvalidModelError(sendErr) {
				// Cached list is stale for this account: drop it and fail over to
				// another account that serves the model.
				s.invalidateModels(creds.id)
				break
			}

			if !isAccountFailure(sendErr) {
				// A problem with the request itself: don't burn other accounts.
				return nil, sendErr
			}
			if isAccountDepleted(sendErr) {
				// Out of credit: park until reset_at/fallback so the account is
				// not retried every 60s. The probe refines this to reset_at.
				s.selector.markDepleted(creds.id, time.Now().Add(depletedFallbackTTL))
			} else {
				s.selector.recordFailure(creds.id)
			}
			break
		}
	}
	if lastErr == nil {
		if modelSkipped {
			return nil, errModelUnavailable
		}
		lastErr = errNoAccount
	}
	return nil, lastErr
}

// firstFrameFailure primes the stream's first event and reports a retriable
// error when the upstream fails before producing any content. It returns nil
// when the stream is healthy (the peeked event is replayed by the first Recv).
//
// Two cases are treated as pre-stream failures worth failing over on:
//   - an in-stream exception frame arriving as the very first event, and
//   - a transport error (other than a clean io.EOF) reading that first frame.
//
// A clean io.EOF is an empty-but-valid stream and passes through unchanged.
// ValidationException (including THINKING_SIGNATURE_INVALID, PROMPT_TOO_LONG) is
// a request-level error; other exceptions remain account failures.
func firstFrameFailure(stream *kiroStream) error {
	ev, err := stream.peekFirst()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("upstream stream failed before any content: %w", err)
	}
	if ev != nil && ev.Kind == evError {
		status := http.StatusBadGateway
		switch ev.ErrKind {
		case "ValidationException":
			status = http.StatusBadRequest
		case "serviceQuotaExceededError":
			status = http.StatusPaymentRequired // 402 – credit exhausted
		}
		return &kiroHTTPError{Status: status, Body: upstreamEventError(ev), ReasonCode: ev.ErrReason, Kind: ev.ErrKind}
	}
	return nil
}

// sendWithReasoningRetry sends the request and, if the backend rejects a stale
// or invalid extended-thinking signature, retries once with reasoningContent
// stripped from history. This mirrors Kiro's own recovery and matters most
// during multi-turn / context compaction, where a signature may no longer
// validate.
func (s *Server) sendWithReasoningRetry(ctx context.Context, creds kiroCredentials, kreq *kiroRequest) (*kiroStream, error) {
	return withReasoningRetry(kreq, func(k *kiroRequest) (*kiroStream, error) {
		return s.kiro.Send(ctx, creds, k)
	})
}

// withReasoningRetry runs send(kreq) and, on an invalid/stale thinking-signature
// rejection, retries once with reasoningContent stripped from history. The retry
// only fires when there was reasoning in history to strip, so an unrelated 400
// (or a signature error with nothing to strip) surfaces unchanged.
//
// This recovers only from a pre-stream validation failure (a non-2xx
// THINKING_SIGNATURE_INVALID, which is how the backend rejects a bad signature
// before streaming begins). Were the same condition ever surfaced as an
// in-stream exception after a 200, it would reach the caller mid-stream, where
// a transparent retry is no longer possible.
func withReasoningRetry(kreq *kiroRequest, send func(*kiroRequest) (*kiroStream, error)) (*kiroStream, error) {
	stream, err := send(kreq)
	if err == nil {
		return stream, nil
	}
	if isThinkingSignatureError(err) && stripReasoningFromHistory(kreq) {
		return send(kreq)
	}
	return nil, err
}

// isPromptTooLongError reports an exact machine-coded context-size rejection.
// Text matching is intentionally avoided so unrelated validation errors cannot
// silently discard conversation history.
func isPromptTooLongError(err error) bool {
	he, ok := err.(*kiroHTTPError)
	return ok && he.Status == http.StatusBadRequest && he.reason() == "PROMPT_TOO_LONG"
}

// isThinkingSignatureError reports whether err is a request-validation failure
// caused by an invalid or stale extended-thinking signature.
func isThinkingSignatureError(err error) bool {
	he, ok := err.(*kiroHTTPError)
	if !ok || he.Status != http.StatusBadRequest {
		return false
	}
	if he.reason() == "THINKING_SIGNATURE_INVALID" {
		return true
	}
	// Fallback when the machine reason is absent: sniff the message text.
	body := strings.ToLower(he.Body)
	return strings.Contains(body, "thinking") && strings.Contains(body, "signature")
}

// isInvalidModelError reports whether the runtime rejected the request because
// the modelId is not served (by this account/region). Unlike other 400s this is
// recoverable: the cached model list may be stale, so the caller invalidates it
// and fails over to another account that serves the model.
func isInvalidModelError(err error) bool {
	he, ok := err.(*kiroHTTPError)
	if !ok || he.Status != http.StatusBadRequest {
		return false
	}
	if he.reason() == "INVALID_MODEL_ID" {
		return true
	}
	// Text fallback only when the machine reason is absent. Require the specific
	// phrase so an unrelated 400 that merely says "invalid model …" isn't
	// misclassified and turned into account failover.
	body := strings.ToLower(he.Body)
	return strings.Contains(body, "invalid model id")
}

// aggregateMessages handles non-streaming requests: collect all events, then
// return a single Anthropic message.
func (s *Server) aggregateMessages(w http.ResponseWriter, r *http.Request, areq *anthropicRequest, stream *kiroStream, inputChars int) {
	asm := newBlockAssembler(nil, toolNameMapFor(areq.Tools))
	asm.emitThinking = !thinkingSuppressed(areq)

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Client disconnected before the aggregated response was ready: record
			// it quietly rather than as a server error, and skip the write (the
			// connection is gone).
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				noteCanceled(r.Context())
				return
			}
			writeAnthropicError(w, r, http.StatusBadGateway, "api_error", "stream read error: "+err.Error())
			return
		}
		switch ev.Kind {
		case evReasoning:
			_ = asm.addReasoning(ev)
		case evText:
			_ = asm.ingestText(ev.Text)
		case evToolUse:
			_ = asm.addToolUse(ev)
		case evMetadata:
			asm.setStopReason(ev.StopReason)
		case evError:
			writeAnthropicError(w, r, http.StatusBadGateway, "api_error", upstreamEventError(ev))
			return
		}
	}
	_ = asm.finish()

	blocks := asm.blocks
	if len(blocks) == 0 {
		blocks = []anthropicRespBlock{{Type: "text", Text: ""}}
	}

	resp := anthropicResponse{
		ID:         "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Type:       "message",
		Role:       "assistant",
		Model:      mapModel(areq.Model),
		Content:    blocks,
		StopReason: asm.stopReason(),
		Usage: anthropicUsage{
			InputTokens:  estimateTokens(inputChars),
			OutputTokens: estimateTokens(asm.outputChars()),
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// streamMessages handles streaming requests, emitting Anthropic SSE events.
func (s *Server) streamMessages(w http.ResponseWriter, r *http.Request, areq *anthropicRequest, stream *kiroStream, inputChars int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, r, http.StatusInternalServerError, "api_error", "streaming unsupported by server")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, data any) error {
		payload, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	// message_start
	if err := emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         mapModel(areq.Model),
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  estimateTokens(inputChars),
				"output_tokens": 0,
			},
		},
	}); err != nil {
		return
	}
	_ = emit("ping", map[string]any{"type": "ping"})

	asm := newBlockAssembler(emit, toolNameMapFor(areq.Tools))
	asm.emitThinking = !thinkingSuppressed(areq)

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// The request context is canceled when the client disconnects; the
			// upstream read then fails with context.Canceled. That is the client
			// going away, not a server error, and there is no live connection
			// left to emit onto, so just record it quietly and stop.
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				noteCanceled(r.Context())
				return
			}
			noteError(r.Context(), "stream read error: "+err.Error())
			_ = emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": "stream read error: " + err.Error()},
			})
			return
		}
		switch ev.Kind {
		case evReasoning:
			if err := asm.addReasoning(ev); err != nil {
				return
			}
		case evText:
			if err := asm.ingestText(ev.Text); err != nil {
				return
			}
		case evToolUse:
			if err := asm.addToolUse(ev); err != nil {
				return
			}
		case evMetadata:
			asm.setStopReason(ev.StopReason)
		case evError:
			noteError(r.Context(), upstreamEventError(ev))
			_ = emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": upstreamEventError(ev)},
			})
			return
		}
	}
	if err := asm.finish(); err != nil {
		return
	}

	// message_delta with final stop reason + output usage.
	_ = emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   asm.stopReason(),
			"stop_sequence": nil,
		},
		"usage": map[string]any{"output_tokens": estimateTokens(asm.outputChars())},
	})
	_ = emit("message_stop", map[string]any{"type": "message_stop"})
}

// authorized enforces the optional local API key.
func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.APIKey == "" {
		return true
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return k == s.cfg.APIKey
	}
	if a := r.Header.Get("Authorization"); a != "" {
		// The scheme is case-insensitive per RFC 7235; accept "Bearer", "bearer", etc.
		if len(a) >= 7 && strings.EqualFold(a[:7], "Bearer ") {
			a = a[7:]
		}
		return a == s.cfg.APIKey
	}
	return false
}

// upstreamEventError renders a Kiro error event as a message.
func upstreamEventError(ev *kiroEvent) string {
	if ev.ErrKind != "" && ev.ErrMsg != "" {
		return fmt.Sprintf("%s: %s", ev.ErrKind, ev.ErrMsg)
	}
	if ev.ErrMsg != "" {
		return ev.ErrMsg
	}
	if ev.ErrKind != "" {
		return ev.ErrKind
	}
	return "unknown upstream error"
}

// mapUpstreamError maps a Kiro HTTP error to an Anthropic status + error type.
func mapUpstreamError(err error) (int, string) {
	if he, ok := err.(*kiroHTTPError); ok {
		switch he.Status {
		case http.StatusUnauthorized:
			return http.StatusUnauthorized, "authentication_error"
		case http.StatusForbidden:
			return http.StatusForbidden, "permission_error"
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "rate_limit_error"
		case http.StatusBadRequest:
		return http.StatusBadRequest, "invalid_request_error"
	case http.StatusPaymentRequired: // 402 – credit exhausted
		return http.StatusPaymentRequired, "api_error"
	case http.StatusLocked: // 423 – account suspended
		return http.StatusLocked, "api_error"
	default:
		return http.StatusBadGateway, "api_error"
		}
	}
	return http.StatusBadGateway, "api_error"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAnthropicError(w http.ResponseWriter, r *http.Request, status int, errType, message string) {
	noteError(r.Context(), message)
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

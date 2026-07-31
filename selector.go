package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Per-request credentials and multi-account selection.
//
// KiroClient.Send and the control-plane calls are written against the
// kiroCredentials interface so they act as a specific account (accountCreds),
// chosen round-robin by accountSelector.
// ---------------------------------------------------------------------------

// kiroCredentials supplies the bearer token, region and profileArn for one
// Kiro call, and can refresh its token on a 401/403.
type kiroCredentials interface {
	// accessToken returns a currently-valid bearer token, refreshing if needed.
	accessToken(ctx context.Context) (string, error)
	// apiRegion is the region hosting the Kiro runtime/management endpoints.
	apiRegion() string
	// profileArn resolves the CodeWhisperer profileArn for the request.
	profileArn(ctx context.Context) (string, error)
	// machineID returns a stable per-account fingerprint for the User-Agent,
	// so pooled accounts look like distinct Kiro IDE installs rather than one
	// host running many sessions. Empty means "no fingerprint".
	machineID() string
	// refresh unconditionally refreshes the token (used to recover from 401/403).
	refresh(ctx context.Context) error
}

// accountCreds serves a request from one stored (self-managed) account. Token
// refresh uses the account's own client registration and is written back to the
// account store; it never touches Kiro's on-disk cache.
type accountCreds struct {
	store  *AccountStore
	client *http.Client
	id     string
	acct   StoredAccount
}

func (c *accountCreds) apiRegion() string {
	if c.acct.Region != "" {
		return c.acct.Region
	}
	return "us-east-1"
}

// machineID returns a stable fingerprint for this account, embedded in the
// User-Agent so each pooled account looks like its own Kiro IDE install.
func (c *accountCreds) machineID() string { return machineIDFor(c.id) }

func (c *accountCreds) profileArn(ctx context.Context) (string, error) {
	if c.acct.ProfileArn != "" {
		return c.acct.ProfileArn, nil
	}
	// Resolve best-effort onto this request's account copy (accounts normally
	// already carry a ProfileArn, so this is a rare fallback path). Use a fresh
	// token via accessToken() rather than the possibly-stale stored one, so an
	// account with an empty profileArn but a valid refreshToken self-heals
	// instead of failing ListAvailableProfiles with a 401. (accessToken() does
	// not call profileArn(), so there is no recursion.)
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn for account %s: %w", c.id, err)
	}
	arn, err := resolveProfileArn(ctx, c.client, c.apiRegion(), tok)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn for account %s: %w", c.id, err)
	}
	c.acct.ProfileArn = arn
	// Persist the resolved profileArn so it survives restarts and shows up on
	// the admin page. Best-effort: a write failure does not affect this request.
	_ = c.store.UpdateIdentity(c.id, arn, "", "")
	// While we have a valid token, also resolve email and userId best-effort and
	// persist them for the same reason (and to backfill userId onto older records
	// that predate the field). fetchAccountIdentity is non-fatal on failure.
	if c.acct.Email == "" || c.acct.UserID == "" {
		ident := fetchAccountIdentity(ctx, c.client, c.apiRegion(), arn, tok)
		if ident.Email != "" {
			c.acct.Email = ident.Email
		}
		if ident.UserID != "" {
			c.acct.UserID = ident.UserID
		}
		if ident.Email != "" || ident.UserID != "" {
			_ = c.store.UpdateIdentity(c.id, "", ident.Email, ident.UserID)
		}
	}
	return arn, nil
}

func (c *accountCreds) accessToken(ctx context.Context) (string, error) {
	// Refresh proactively when the token is missing or near expiry. The
	// background refresher normally keeps it fresh; this covers the gap.
	exp := c.acct.expiry()
	needs := c.acct.AccessToken == "" ||
		(!exp.IsZero() && time.Now().Add(tokenRefreshBuffer).After(exp))
	if needs {
		if err := c.refresh(ctx); err != nil {
			// If we still hold a token, use it and let the backend decide.
			if c.acct.AccessToken != "" {
				return c.acct.AccessToken, nil
			}
			return "", err
		}
	}
	return c.acct.AccessToken, nil
}

func (c *accountCreds) refresh(ctx context.Context) error {
	// Route through the store so concurrent refreshes of this account (across
	// requests and the background refresher) collapse into one CreateToken call.
	fresh, err := c.store.RefreshToken(ctx, c.client, c.id)
	if err != nil {
		return err
	}
	c.acct = fresh
	return nil
}

// ---------------------------------------------------------------------------
// Account selector: round-robin over the store with a lightweight cooldown.
// ---------------------------------------------------------------------------

// accountCooldown is how long a failing account is skipped before it is retried.
const accountCooldown = 60 * time.Second

// depletedFallbackTTL is how long an account whose credit is exhausted is
// skipped when the precise reset time is unknown — a request's quota error
// carries no reset_at. It must exceed depletedProbeInterval so the background
// probe can refine the deadline to the real reset_at before the fallback
// expires and traffic retries the account.
const depletedFallbackTTL = 45 * time.Minute

// depletedEntry tracks one parked account. until is when credit is expected
// back (reset_at or fallback); basis is the moment the decision is grounded in
// (the request-failure time, or the usage snapshot's fetch time). Writes whose
// basis predates the current entry are ignored, so a stale usage snapshot
// cannot override a fresher request-failure signal; until never moves earlier,
// so a fallback deadline cannot clobber a precise reset_at.
type depletedEntry struct {
	until time.Time
	basis time.Time
}

// accountSelector picks accounts round-robin, skipping those in cooldown or
// marked depleted (credit exhausted until a reset/upgrade restores it).
type accountSelector struct {
	store  *AccountStore
	client *http.Client

	mu       sync.Mutex
	index    int
	cooldown map[string]time.Time     // account id -> time the cooldown ends
	depleted map[string]depletedEntry // account id -> when credit returns + the basis for that claim
}

func newAccountSelector(store *AccountStore, client *http.Client) *accountSelector {
	return &accountSelector{
		store:    store,
		client:   client,
		cooldown: map[string]time.Time{},
		depleted: map[string]depletedEntry{},
	}
}

// pick returns credentials for the next eligible account, skipping ids in tried
// and accounts currently in cooldown. When every remaining account is in
// cooldown it falls back to the one whose cooldown ends soonest. ok is false
// only when there are no accounts left to try (store empty or all tried).
func (s *accountSelector) pick(tried map[string]bool) (*accountCreds, bool) {
	list := s.store.List()
	if len(list) == 0 {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneCooldownLocked(list)

	// skipUntil reports whether an account is temporarily skipped right now
	// (cooldown or depleted) and the time it recovers. Both are tracked so the
	// request always has a fallback when every account is momentarily unusable.
	// skipUntil reports whether an account is temporarily skipped (cooldown or
	// depleted) and when it fully recovers — the later of the two deadlines,
	// since both must lift before the account is usable. The fallback ranks
	// candidates by true recovery time, not whichever skip fired first.
	skipUntil := func(id string) (time.Time, bool) {
		rec := time.Time{}
		skipped := false
		if cu, ok := s.cooldown[id]; ok && now.Before(cu) {
			rec = cu
			skipped = true
		}
		if de, ok := s.depleted[id]; ok && now.Before(de.until) {
			if de.until.After(rec) {
				rec = de.until
			}
			skipped = true
		}
		return rec, skipped
	}

	// Round-robin scan for an account that is neither tried nor skipped.
	var soonest *StoredAccount
	var soonestAt time.Time
	n := len(list)
	for i := 0; i < n; i++ {
		a := list[(s.index+i)%n]
		if tried[a.ID] {
			continue
		}
		if !accountUsable(a) {
			continue // dead account: no profileArn and no way to obtain one
		}
		if until, skip := skipUntil(a.ID); skip {
			// Track the candidate that recovers soonest as a fallback.
			if soonest == nil || until.Before(soonestAt) {
				ac := a
				soonest, soonestAt = &ac, until
			}
			continue
		}
		s.index = (s.index + i + 1) % n
		return s.credsFor(a), true
	}

	// Everything untried is skipped: use the soonest-recovering account.
	if soonest != nil {
		return s.credsFor(*soonest), true
	}
	return nil, false
}

// credsFor builds accountCreds for the given account snapshot.
func (s *accountSelector) credsFor(a StoredAccount) *accountCreds {
	return &accountCreds{store: s.store, client: s.client, id: a.ID, acct: a}
}

// accountUsable reports whether an account can serve a request at all: it must
// not be administratively parked (Disabled), and it must either already carry a
// profileArn, or hold a full client registration plus refresh token so
// accessToken()/profileArn() can refresh and resolve one. An account with
// neither (e.g. an import whose profileArn lookup failed and that lacks
// credentials to recover) is dead weight and is skipped during selection.
// Disabled accounts are still stored, refreshed and shown on the admin page;
// this gate only excludes them from selection.
func accountUsable(a StoredAccount) bool {
	if a.Disabled {
		return false
	}
	if a.ProfileArn != "" {
		return true
	}
	return a.RefreshToken != "" && a.ClientID != "" && a.ClientSecret != ""
}

// pruneCooldownLocked drops cooldown/depleted entries for accounts no longer in
// the store, so removed accounts do not leak entries. Caller must hold s.mu.
func (s *accountSelector) pruneCooldownLocked(list []StoredAccount) {
	if len(s.cooldown) == 0 && len(s.depleted) == 0 {
		return
	}
	live := make(map[string]bool, len(list))
	for _, a := range list {
		live[a.ID] = true
	}
	for id := range s.cooldown {
		if !live[id] {
			delete(s.cooldown, id)
		}
	}
	for id := range s.depleted {
		if !live[id] {
			delete(s.depleted, id)
		}
	}
}

// peekAny returns credentials for one eligible account for a control-plane call
// (model schema lookup) that is not tied to a specific request. Unlike pick it
// does NOT advance the round-robin cursor, so schema lookups do not perturb
// request dispatch fairness (which matters especially with few accounts). It
// skips unusable accounts and prefers one not in cooldown, falling back to the
// first usable account. ok is false when no usable account exists.
func (s *accountSelector) peekAny() (*accountCreds, bool) {
	list := s.store.List()
	if len(list) == 0 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var firstUsable *StoredAccount
	for i := range list {
		a := list[i]
		if !accountUsable(a) {
			continue
		}
		if firstUsable == nil {
			firstUsable = &list[i]
		}
		if until, ok := s.cooldown[a.ID]; ok && now.Before(until) {
			continue
		}
		return s.credsFor(a), true
	}
	// All usable accounts are cooling down: any of them serves a (cached)
	// schema lookup. If none are usable at all, report no account.
	if firstUsable != nil {
		return s.credsFor(*firstUsable), true
	}
	return nil, false
}

// byID returns credentials for one specific account by id, regardless of its
// usability/cooldown state — used by per-account control-plane calls that the
// admin explicitly targets (e.g. "show models for THIS account's card"). ok is
// false when the id is unknown.
func (s *accountSelector) byID(id string) (*accountCreds, bool) {
	for _, a := range s.store.List() {
		if a.ID == id {
			return s.credsFor(a), true
		}
	}
	return nil, false
}

// listAll returns credentials for every stored account, for per-account
// control-plane calls (e.g. rendering usage for each account on the admin page).
func (s *accountSelector) listAll() []*accountCreds {
	list := s.store.List()
	out := make([]*accountCreds, 0, len(list))
	for _, a := range list {
		out = append(out, s.credsFor(a))
	}
	return out
}

// recordSuccess clears any cooldown and depleted mark on the account. A depleted
// account only reaches the success path via the all-skipped fallback; if it then
// serves the request, it plainly has credit again.
func (s *accountSelector) recordSuccess(id string) {
	s.mu.Lock()
	delete(s.cooldown, id)
	delete(s.depleted, id)
	s.mu.Unlock()
}

// recordFailure puts the account into cooldown.
func (s *accountSelector) recordFailure(id string) {
	s.mu.Lock()
	s.cooldown[id] = time.Now().Add(accountCooldown)
	s.mu.Unlock()
}

// markDepleted skips an account for credit-bearing requests until the given
// time (a fallback TTL — request errors carry no reset_at). pick consults this;
// peekAny ignores it because schema/model lookups do not consume credit. The
// deadline never moves earlier, so it cannot clobber a precise reset_at that a
// probe/admin refresh already recorded.
func (s *accountSelector) markDepleted(id string, until time.Time) {
	s.mu.Lock()
	s.setDepletedLocked(id, until, time.Now())
	s.mu.Unlock()
}

// clearDepleted lifts a depleted mark immediately.
func (s *accountSelector) clearDepleted(id string) {
	s.mu.Lock()
	delete(s.depleted, id)
	s.mu.Unlock()
}

// setDepletedLocked parks id until at least deadline, grounded at basis. A
// write whose basis predates the existing entry's basis is dropped (stale data
// must not override fresher data); the deadline never moves earlier. Caller
// must hold s.mu.
func (s *accountSelector) setDepletedLocked(id string, deadline, basis time.Time) {
	if e, ok := s.depleted[id]; ok {
		if e.basis.After(basis) {
			return // existing entry grounded in fresher information
		}
		if e.until.After(deadline) {
			deadline = e.until // never shorten an existing deadline
		}
	}
	s.depleted[id] = depletedEntry{until: deadline, basis: basis}
}

// depletedDeadline returns when credit is expected back from a usage snapshot:
// its reset_at, or now+depletedFallbackTTL when reset_at is missing or past.
func depletedDeadline(u *kiroUsage, now time.Time) time.Time {
	if u.ResetAt != "" {
		if t, err := time.Parse(time.RFC3339, u.ResetAt); err == nil && t.After(now) {
			return t
		}
	}
	return now.Add(depletedFallbackTTL)
}

// applyUsage reconciles the depleted state with a usage snapshot taken at
// fetched. An account with budget left is un-parked; one without is parked
// until reset_at (or the fallback TTL). Budget means base credit remaining, or
// — only when the account opts in via OverageEnabled — base exhausted but
// overage still available. The overage opt-in only lifts on an authoritative
// path (preciseOnly=false: ensureUsage / admin toggle); the probe path ignores
// it, since an optimistic overage budget under upstream currentUsage clamping
// would lift a fresher reactive 402 mark and loop (probe un-park -> fail ->
// re-park). With overage disabled (the default) zero base credit counts as
// exhausted, preserving the legacy strictly-no-overage behaviour.
// preciseOnly (the probe path) refines an existing entry but never creates one,
// so a stale snapshot cannot re-park an account that just recovered and served
// traffic. Writes are grounded at fetched, so a stale snapshot cannot lift a
// mark a concurrent request failure just set. A nil usage/credit leaves the
// state untouched.
func (s *accountSelector) applyUsage(id string, u *kiroUsage, fetched time.Time, preciseOnly bool) {
	if u == nil || u.Credit == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	budget := u.Credit.Remaining > 0
	if !budget && !preciseOnly {
		// Base exhausted: overage may still serve, but only if the account opts
		// in. Skipped on the probe path (preciseOnly), where an optimistic
		// overage budget must not lift a fresher reactive 402 mark.
		if a, ok := s.store.Get(id); ok && a.OverageEnabled && overageRemaining(u.Credit) > 0 {
			budget = true
		}
	}
	if budget {
		// Lift the mark only if this snapshot is at least as fresh as the mark.
		if e, ok := s.depleted[id]; !ok || !e.basis.After(fetched) {
			delete(s.depleted, id)
		}
		return
	}
	if preciseOnly {
		if _, ok := s.depleted[id]; !ok {
			return // do not re-park an account that recovered since the snapshot
		}
	}
	s.setDepletedLocked(id, depletedDeadline(u, time.Now()), fetched)
}

// overageRemaining reports how much overage budget is left on a CREDIT usage
// line. Once Used exceeds Limit the account is spending overage, so the excess
// is overage spent. Robust to either upstream currentUsage semantics: when the
// upstream clamps currentUsage at the limit, this stays positive and the
// reactive markDepleted path catches a truly exhausted account instead.
func overageRemaining(c *kiroCreditUsage) float64 {
	if c == nil || c.OverageCap <= 0 {
		return 0
	}
	spent := c.Used - c.Limit
	if spent < 0 {
		spent = 0
	}
	if r := c.OverageCap - spent; r > 0 {
		return r
	}
	return 0
}

// depletedIDs returns a snapshot of ids currently marked depleted, for the
// background probe to re-check. Callers need not hold the lock.
func (s *accountSelector) depletedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.depleted))
	for id := range s.depleted {
		ids = append(ids, id)
	}
	return ids
}

// isAccountFailure reports whether err is an upstream failure that warrants
// trying a different account (auth, quota/throttle, upstream 5xx, or a
// transport error). A 400 that is not a thinking-signature issue is a problem
// with the request itself and should surface immediately without burning other
// accounts.
func isAccountFailure(err error) bool {
	if err == nil {
		return false
	}
	he, ok := err.(*kiroHTTPError)
	if !ok {
		// Transport/stream error (e.g. connection reset): try another account.
		return true
	}
	switch {
	case he.Status == http.StatusUnauthorized, he.Status == http.StatusForbidden:
		return true
	case he.Status == http.StatusPaymentRequired, he.Status == http.StatusTooManyRequests:
		return true
	case he.Status == 423: // Locked (account suspended)
		return true
	case he.Status >= 500:
		return true
	default:
		return false
	}
}

// isAccountDepleted reports whether err signals the account is out of credit:
// HTTP 402 (payment required) or 423 (locked/suspended), or an in-stream
// serviceQuotaExceededError event (surfaced via the error's Kind). Such errors
// park the account in the depleted map until reset_at/fallback rather than the
// short cooldown, so it is not retried every 60s. It is a strict subset of
// isAccountFailure; a 429 throttle stays on the short cooldown.
func isAccountDepleted(err error) bool {
	if err == nil {
		return false
	}
	he, ok := err.(*kiroHTTPError)
	if !ok {
		return false
	}
	if he.Status == http.StatusPaymentRequired || he.Status == http.StatusLocked {
		return true
	}
	return he.Kind == "serviceQuotaExceededError"
}

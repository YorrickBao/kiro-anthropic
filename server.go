package main

import (
	"context"
	"encoding/json"
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
)

// Server wires the Anthropic-facing HTTP API to the Kiro backend.
type Server struct {
	cfg      *Config
	store    *TokenStore
	kiro     *KiroClient
	accounts *AccountStore // multi-account credential store (admin sign-in)
	login    *loginManager // IdC sign-in flow driver
	logger   *slog.Logger  // per-request access log; nil disables it

	modelsMu    sync.Mutex
	modelsCache []kiroModelInfo

	usageMu      sync.Mutex
	usageCache   *kiroUsage
	usageFetched time.Time
}

// usageCacheTTL is how long a GetUsageLimits result is reused before refetching,
// so the auto-refreshing admin page does not hammer the control plane.
const usageCacheTTL = 60 * time.Second

// fallbackModels is used for /v1/models if the live ListAvailableModels call fails.
var fallbackModels = []string{
	"auto", "claude-sonnet-5", "claude-opus-4.8", "claude-opus-4.7", "claude-opus-4.6",
	"claude-sonnet-4.6", "claude-opus-4.5", "claude-sonnet-4.5", "claude-sonnet-4", "claude-haiku-4.5",
}

func NewServer(cfg *Config, store *TokenStore, client *http.Client) *Server {
	return &Server{
		cfg:   cfg,
		store: store,
		kiro:  NewKiroClient(cfg, store, client),
		login: newLoginManager(client),
	}
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
	return r
}

// ---- request logging ----

// ctxKeyAccess is the context key under which per-request log details live.
type ctxKeyAccess struct{}

// accessInfo carries request details that handlers attach for the access log.
type accessInfo struct {
	model  string
	stream bool
	errMsg string // failure cause, recorded when the request errors
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
	if models := s.ensureModels(r.Context()); len(models) > 0 {
		for _, m := range models {
			data = append(data, modelInfoJSON(m, now))
		}
	} else {
		// Fallback when the control plane is unreachable: ids only.
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

// ensureModels fetches the account's models once and caches them.
func (s *Server) ensureModels(ctx context.Context) []kiroModelInfo {
	s.modelsMu.Lock()
	cached := s.modelsCache
	s.modelsMu.Unlock()
	if cached != nil {
		return cached
	}
	models, err := s.kiro.ListModels(ctx)
	if err != nil || len(models) == 0 {
		return nil // don't cache a failure; retry next time
	}
	s.modelsMu.Lock()
	s.modelsCache = models
	s.modelsMu.Unlock()
	return models
}

// ensureUsage returns the account usage, fetching it at most once per
// usageCacheTTL. A failed fetch is not cached (it retries next call) and is
// surfaced to the caller.
func (s *Server) ensureUsage(ctx context.Context) (*kiroUsage, error) {
	s.usageMu.Lock()
	if s.usageCache != nil && time.Since(s.usageFetched) < usageCacheTTL {
		u := s.usageCache
		s.usageMu.Unlock()
		return u, nil
	}
	s.usageMu.Unlock()

	u, err := s.kiro.GetUsage(ctx)
	if err != nil {
		return nil, err
	}
	s.usageMu.Lock()
	s.usageCache = u
	s.usageFetched = time.Now()
	s.usageMu.Unlock()
	return u, nil
}

// availableModelIDs returns the account's model IDs (static fallback on error).
func (s *Server) availableModelIDs(r *http.Request) []string {
	models := s.ensureModels(r.Context())
	if len(models) == 0 {
		return fallbackModels
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ModelID != "" {
			ids = append(ids, m.ModelID)
		}
	}
	if len(ids) == 0 {
		return fallbackModels
	}
	return ids
}

// modelInfo returns the cached info for a model id.
func (s *Server) modelInfo(ctx context.Context, modelID string) (kiroModelInfo, bool) {
	for _, m := range s.ensureModels(ctx) {
		if m.ModelID == modelID {
			return m, true
		}
	}
	return kiroModelInfo{}, false
}

// applyModelRequestFields sets reasoning effort and max output tokens on the
// outgoing request. Both are request-driven with a "top out" default: if the
// client specified a value it is honored (clamped to the model), otherwise the
// model's maximum is used. Only fields the model's schema actually declares are
// set, so models without them (e.g. "auto", claude-sonnet-4.5) are left
// untouched and never rejected.
func (s *Server) applyModelRequestFields(ctx context.Context, kreq *kiroRequest, reqEffort string, reqMaxTokens int) {
	um := kreq.ConversationState.CurrentMessage.UserInputMessage
	if um == nil || um.ModelID == "" || um.ModelID == "auto" {
		return
	}
	model, ok := s.modelInfo(ctx, um.ModelID)
	if !ok {
		return
	}

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
	newImageFetcher(s.kiro.client).resolveRemoteImages(r.Context(), &areq)

	kreq, err := buildKiroRequest(s.cfg, &areq)
	if err != nil {
		writeAnthropicError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Resolve the profileArn required by the runtime.
	arn, err := s.store.ProfileArn(r.Context())
	if err != nil {
		writeAnthropicError(w, r, http.StatusBadGateway, "api_error", "could not resolve profileArn: "+err.Error())
		return
	}
	kreq.ProfileArn = arn

	// Effort and max output tokens: request-driven, defaulting to the model max.
	s.applyModelRequestFields(r.Context(), kreq, requestedEffort(&areq), areq.MaxTokens)

	stream, err := s.sendWithReasoningRetry(r.Context(), kreq)
	if err != nil {
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

// sendWithReasoningRetry sends the request and, if the backend rejects a stale
// or invalid extended-thinking signature, retries once with reasoningContent
// stripped from history. This mirrors Kiro's own recovery and matters most
// during multi-turn / context compaction, where a signature may no longer
// validate.
func (s *Server) sendWithReasoningRetry(ctx context.Context, kreq *kiroRequest) (*kiroStream, error) {
	return withReasoningRetry(kreq, func(k *kiroRequest) (*kiroStream, error) {
		return s.kiro.Send(ctx, k)
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

// aggregateMessages handles non-streaming requests: collect all events, then
// return a single Anthropic message.
func (s *Server) aggregateMessages(w http.ResponseWriter, r *http.Request, areq *anthropicRequest, stream *kiroStream, inputChars int) {
	asm := newBlockAssembler(nil)
	asm.emitThinking = !thinkingSuppressed(areq)

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
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

	asm := newBlockAssembler(emit)
	asm.emitThinking = !thinkingSuppressed(areq)

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
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
		return strings.TrimPrefix(a, "Bearer ") == s.cfg.APIKey
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

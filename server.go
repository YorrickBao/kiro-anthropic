package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server wires the Anthropic-facing HTTP API to the Kiro backend.
type Server struct {
	cfg   *Config
	store *TokenStore
	kiro  *KiroClient

	modelsMu    sync.Mutex
	modelsCache []kiroModelInfo
}

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
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	return mux
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
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
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
	// maximum when unspecified. Always clamped to the advertised levels.
	if ec, ok := model.effort(); ok {
		desired := strings.ToLower(strings.TrimSpace(reqEffort))
		if desired == "" {
			desired = "max"
		}
		if level := ec.clamp(desired); level != "" {
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
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.authorized(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var areq anthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	kreq, err := buildKiroRequest(s.cfg, &areq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Resolve the profileArn required by the runtime.
	arn, err := s.store.ProfileArn(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not resolve profileArn: "+err.Error())
		return
	}
	kreq.ProfileArn = arn

	// Effort and max output tokens: request-driven, defaulting to the model max.
	s.applyModelRequestFields(r.Context(), kreq, requestedEffort(&areq), areq.MaxTokens)

	stream, err := s.kiro.Send(r.Context(), kreq)
	if err != nil {
		status, errType := mapUpstreamError(err)
		writeAnthropicError(w, status, errType, err.Error())
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

// aggregateMessages handles non-streaming requests: collect all events, then
// return a single Anthropic message.
func (s *Server) aggregateMessages(w http.ResponseWriter, r *http.Request, areq *anthropicRequest, stream *kiroStream, inputChars int) {
	asm := newBlockAssembler(nil)

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "stream read error: "+err.Error())
			return
		}
		switch ev.Kind {
		case evText:
			_ = asm.addText(ev.Text)
		case evToolUse:
			_ = asm.addToolUse(ev)
		case evError:
			writeAnthropicError(w, http.StatusBadGateway, "api_error", upstreamEventError(ev))
			return
		}
	}
	_ = asm.closeOpen()

	blocks := asm.blocks
	if len(blocks) == 0 {
		blocks = []anthropicRespBlock{{Type: "text", Text: ""}}
	}

	resp := anthropicResponse{
		ID:         "msg_" + strings.ReplaceAll(newUUID(), "-", ""),
		Type:       "message",
		Role:       "assistant",
		Model:      areq.Model,
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
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming unsupported by server")
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

	msgID := "msg_" + strings.ReplaceAll(newUUID(), "-", "")

	// message_start
	if err := emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         areq.Model,
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

	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": "stream read error: " + err.Error()},
			})
			return
		}
		switch ev.Kind {
		case evText:
			if err := asm.addText(ev.Text); err != nil {
				return
			}
		case evToolUse:
			if err := asm.addToolUse(ev); err != nil {
				return
			}
		case evError:
			_ = emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": upstreamEventError(ev)},
			})
			return
		}
	}
	if err := asm.closeOpen(); err != nil {
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

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

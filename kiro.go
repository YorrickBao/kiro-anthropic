package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Kiro / CodeWhisperer streaming wire types (GenerateAssistantResponse).
// Field names and nesting come from Kiro's bundled client.
// ---------------------------------------------------------------------------

type kiroRequest struct {
	ConversationState            kiroConversationState `json:"conversationState"`
	ProfileArn                   string                `json:"profileArn,omitempty"`
	AgentMode                    string                `json:"agentMode,omitempty"`
	SystemPrompt                 string                `json:"systemPrompt,omitempty"`
	AdditionalModelRequestFields map[string]any        `json:"additionalModelRequestFields,omitempty"`
}

type kiroConversationState struct {
	ChatTriggerType string        `json:"chatTriggerType"`
	ConversationID  string        `json:"conversationId"`
	CurrentMessage  kiroMessage   `json:"currentMessage"`
	History         []kiroMessage `json:"history,omitempty"`
}

// kiroMessage is a union: exactly one of the two pointers is set.
type kiroMessage struct {
	UserInputMessage         *kiroUserInputMessage `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *kiroAssistantMessage `json:"assistantResponseMessage,omitempty"`
}

type kiroUserInputMessage struct {
	Content                 string                       `json:"content"`
	ModelID                 string                       `json:"modelId,omitempty"`
	Origin                  string                       `json:"origin,omitempty"`
	UserInputMessageContext *kiroUserInputMessageContext `json:"userInputMessageContext,omitempty"`
	Images                  []kiroImage                  `json:"images,omitempty"`
}

// kiroImage is a CodeWhisperer ImageBlock. Over AWS JSON 1.0 the source bytes
// blob is a base64 string, which matches Anthropic's source.data directly.
type kiroImage struct {
	Format string          `json:"format"` // png | jpeg | gif | webp
	Source kiroImageSource `json:"source"`
}

type kiroImageSource struct {
	Bytes string `json:"bytes"` // base64-encoded image bytes
}

type kiroUserInputMessageContext struct {
	Tools       []kiroTool       `json:"tools,omitempty"`
	ToolResults []kiroToolResult `json:"toolResults,omitempty"`
}

type kiroTool struct {
	ToolSpecification kiroToolSpecification `json:"toolSpecification"`
}

type kiroToolSpecification struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema kiroInputSchema `json:"inputSchema"`
}

type kiroInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type kiroToolResult struct {
	ToolUseID string                  `json:"toolUseId"`
	Content   []kiroToolResultContent `json:"content"`
	Status    string                  `json:"status,omitempty"` // "success" | "error"
}

type kiroToolResultContent struct {
	Text string          `json:"text,omitempty"`
	JSON json.RawMessage `json:"json,omitempty"`
}

type kiroAssistantMessage struct {
	Content          string                `json:"content"`
	ToolUses         []kiroToolUse         `json:"toolUses,omitempty"`
	ReasoningContent *kiroReasoningContent `json:"reasoningContent,omitempty"`
}

// kiroReasoningContent is the CodeWhisperer ReasoningContent union carried on an
// assistant turn in history. Exactly one member is set: reasoningText for normal
// extended thinking (text + signature), or redactedContent for encrypted
// reasoning the backend chose to redact.
type kiroReasoningContent struct {
	ReasoningText   *kiroReasoningText `json:"reasoningText,omitempty"`
	RedactedContent string             `json:"redactedContent,omitempty"` // base64 blob
}

type kiroReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

type kiroToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// ---------------------------------------------------------------------------
// Parsed streaming events (protocol-agnostic view for the translation layer).
// ---------------------------------------------------------------------------

type kiroEventKind int

const (
	evText kiroEventKind = iota
	evReasoning
	evToolUse
	evMetadata
	evError
	evOther
)

type kiroEvent struct {
	Kind kiroEventKind

	Text string // evText

	ReasoningText      string // evReasoning (partial thinking text chunk)
	ReasoningSignature string // evReasoning (thinking signature; arrives once, after text)
	ReasoningRedacted  string // evReasoning (base64 blob for redacted reasoning)

	ToolUseID string // evToolUse
	ToolName  string // evToolUse (present on first chunk)
	ToolInput string // evToolUse (partial JSON chunk)
	ToolStop  bool   // evToolUse (final chunk for this tool use)

	ConversationID string // evMetadata
	StopReason     string // evMetadata (CodeWhisperer stopReason, e.g. END_TURN / TOOL_USE / MAX_TOKENS)

	ErrKind   string // evError
	ErrMsg    string // evError
	ErrReason string // evError (CodeWhisperer ValidationException reason code, e.g. THINKING_SIGNATURE_INVALID)
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// KiroClient sends GenerateAssistantResponse calls to the Kiro runtime and
// exposes the response as a stream of parsed events. It is account-agnostic:
// every call takes a kiroCredentials for the account it should act as.
type KiroClient struct {
	cfg    *Config
	client *http.Client
}

func NewKiroClient(cfg *Config, client *http.Client) *KiroClient {
	return &KiroClient{cfg: cfg, client: client}
}

func (c *KiroClient) runtimeEndpoint(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev/", region)
}

// kiroStream is an open streaming response.
type kiroStream struct {
	resp *http.Response
	dec  *eventStreamDecoder

	// primed holds the first event (and its error) once peekFirst has run, so a
	// caller can inspect it before committing response headers and still have
	// Recv replay it as the first event.
	primed    bool
	primedEv  *kiroEvent
	primedErr error
}

// Send issues the request using the supplied credentials and returns a stream
// of events. On a 401/403 the credentials are refreshed and the request retried
// once.
func (c *KiroClient) Send(ctx context.Context, creds kiroCredentials, req *kiroRequest) (*kiroStream, error) {
	stream, status, body, err := c.sendOnce(ctx, creds, req)
	if err != nil {
		return nil, err
	}
	if stream != nil {
		return stream, nil
	}
	// Non-2xx on first try.
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if rerr := creds.refresh(ctx); rerr == nil {
			stream, status, body, err = c.sendOnce(ctx, creds, req)
			if err != nil {
				return nil, err
			}
			if stream != nil {
				return stream, nil
			}
		}
	}
	return nil, &kiroHTTPError{Status: status, Body: body}
}

// sendOnce performs a single attempt. On success it returns a stream; on a
// non-2xx response it returns (nil, status, body, nil).
func (c *KiroClient) sendOnce(ctx context.Context, creds kiroCredentials, req *kiroRequest) (*kiroStream, int, string, error) {
	token, err := creds.accessToken(ctx)
	if err != nil {
		return nil, 0, "", err
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, 0, "", err
	}

	endpoint := c.runtimeEndpoint(creds.apiRegion())
	if os.Getenv("KIRO_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[kiro-debug] POST %s\n[kiro-debug] body: %s\n", endpoint, payload)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-amz-json-1.0")
	httpReq.Header.Set("X-Amz-Target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", "kiro-anthropic/"+version)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, 0, "", err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := readSnippet(resp.Body)
		resp.Body.Close()
		return nil, resp.StatusCode, body, nil
	}

	return &kiroStream{resp: resp, dec: newEventStreamDecoder(resp.Body)}, resp.StatusCode, "", nil
}

// peekFirst reads and buffers the first event so the caller can inspect it
// before sending response headers. It is idempotent: the buffered event (or
// error) is replayed by the first Recv call. This enables pre-stream failover
// on an upstream exception that arrives as the very first frame, before any
// assistant content has been produced.
func (s *kiroStream) peekFirst() (*kiroEvent, error) {
	if !s.primed {
		s.primedEv, s.primedErr = s.recvRaw()
		s.primed = true
	}
	return s.primedEv, s.primedErr
}

// Recv returns the next parsed event, or io.EOF when the stream ends.
func (s *kiroStream) Recv() (*kiroEvent, error) {
	if s.primed {
		s.primed = false
		ev, err := s.primedEv, s.primedErr
		s.primedEv, s.primedErr = nil, nil
		return ev, err
	}
	return s.recvRaw()
}

// recvRaw reads the next event directly from the decoder, bypassing the peek
// buffer.
func (s *kiroStream) recvRaw() (*kiroEvent, error) {
	msg, err := s.dec.Next()
	if err != nil {
		return nil, err
	}
	if os.Getenv("KIRO_DEBUG_STREAM") != "" {
		fmt.Fprintf(os.Stderr, "[kiro-stream] :message-type=%s :event-type=%s :exception-type=%s payload=%s\n",
			msg.messageType(), msg.eventType(), msg.exceptionType(), msg.payload)
	}
	return parseKiroMessage(msg), nil
}

func (s *kiroStream) Close() error {
	if s.resp != nil && s.resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(s.resp.Body, 4096))
		return s.resp.Body.Close()
	}
	return nil
}

// parseKiroMessage converts a raw framed message into a kiroEvent.
func parseKiroMessage(msg *eventMessage) *kiroEvent {
	// Service-level exceptions are flagged in the message headers.
	if msg.messageType() == "exception" {
		errMsg, reason := extractMessageAndReason(msg.payload)
		return &kiroEvent{Kind: evError, ErrKind: msg.exceptionType(), ErrMsg: errMsg, ErrReason: reason}
	}

	switch msg.eventType() {
	case "assistantResponseEvent":
		var p struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(msg.payload, &p)
		return &kiroEvent{Kind: evText, Text: p.Content}

	case "reasoningContentEvent":
		// Extended-thinking stream: text chunks arrive first, then a single
		// frame carrying the signature; redactedContent appears when the
		// backend encrypts the reasoning. See ReasoningContentEvent in Kiro's
		// bundled CodeWhisperer client.
		var p struct {
			Text            string `json:"text"`
			Signature       string `json:"signature"`
			RedactedContent string `json:"redactedContent"`
		}
		_ = json.Unmarshal(msg.payload, &p)
		return &kiroEvent{
			Kind:               evReasoning,
			ReasoningText:      p.Text,
			ReasoningSignature: p.Signature,
			ReasoningRedacted:  p.RedactedContent,
		}

	case "toolUseEvent":
		var p struct {
			ToolUseID string          `json:"toolUseId"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			Stop      bool            `json:"stop"`
		}
		_ = json.Unmarshal(msg.payload, &p)
		return &kiroEvent{
			Kind:      evToolUse,
			ToolUseID: p.ToolUseID,
			ToolName:  p.Name,
			ToolInput: rawToInputString(p.Input),
			ToolStop:  p.Stop,
		}

	case "metadataEvent":
		// Terminal frame. stopReason is the backend's authoritative finish
		// reason (END_TURN / TOOL_USE / MAX_TOKENS / ...); conversationId is
		// included for completeness though it is usually empty (the live id
		// arrives in the initial-response frame).
		var p struct {
			ConversationID string `json:"conversationId"`
			StopReason     string `json:"stopReason"`
		}
		_ = json.Unmarshal(msg.payload, &p)
		return &kiroEvent{Kind: evMetadata, ConversationID: p.ConversationID, StopReason: p.StopReason}

	case "invalidStateEvent", "internalServerException", "throttlingError",
		"serviceQuotaExceededError", "conversationExpiredError", "dryRunSucceedEvent":
		// Error-ish union members surfaced as normal events.
		errMsg, reason := extractMessageAndReason(msg.payload)
		return &kiroEvent{Kind: evError, ErrKind: msg.eventType(), ErrMsg: errMsg, ErrReason: reason}

	default:
		// codeReferenceEvent, followupPromptEvent, supplementaryWebLinksEvent,
		// contextUsageEvent, etc. are not needed for the Anthropic mapping.
		return &kiroEvent{Kind: evOther}
	}
}

// rawToInputString turns a toolUse "input" chunk into a string. The field may
// arrive as a JSON string chunk ("...") or a raw JSON fragment; both are
// concatenated as text on the Anthropic side.
func rawToInputString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// extractMessageAndReason pulls the human-readable message and the machine
// reason code (CodeWhisperer ValidationException carries {message, reason})
// out of an error payload. reason is "" when absent.
func extractMessageAndReason(payload []byte) (message, reason string) {
	var p struct {
		Message string `json:"message"`
		Msg     string `json:"Message"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(payload, &p); err == nil {
		reason = p.Reason
		switch {
		case p.Message != "":
			return p.Message, reason
		case p.Msg != "":
			return p.Msg, reason
		}
	}
	return string(payload), reason
}

// reason parses the CodeWhisperer reason code out of a non-2xx runtime response
// body (a ValidationException is {"message":...,"reason":...}). Returns "" when
// the body is not JSON or carries no reason.
func (e *kiroHTTPError) reason() string {
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(e.Body), &p); err == nil {
		return p.Reason
	}
	return ""
}

// kiroHTTPError carries a non-2xx runtime response.
type kiroHTTPError struct {
	Status int
	Body   string
}

func (e *kiroHTTPError) Error() string {
	return fmt.Sprintf("kiro runtime returned %d: %s", e.Status, e.Body)
}

// kiroModelInfo describes one model returned by ListAvailableModels.
type kiroModelInfo struct {
	ModelID   string `json:"modelId"`
	ModelName string `json:"modelName"`
	// Some responses flag the account default; we surface it best-effort.
	Default   bool `json:"default"`
	IsDefault bool `json:"isDefault"`
	// Per-model schema describing where reasoning-effort / max_tokens go.
	AdditionalModelRequestFieldsSchema json.RawMessage `json:"additionalModelRequestFieldsSchema"`
	// Context window limits.
	TokenLimits struct {
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxOutputTokens int `json:"maxOutputTokens"`
	} `json:"tokenLimits"`
}

// maxTokensRange reports the [min, max] the model accepts for the additional
// "max_tokens" field, and whether the model accepts it at all (its schema must
// declare max_tokens). Returns ok=false for models with no schema (e.g. "auto",
// claude-sonnet-4.5).
func (m kiroModelInfo) maxTokensRange() (min, max int, ok bool) {
	if len(m.AdditionalModelRequestFieldsSchema) == 0 {
		return 0, 0, false
	}
	var schema struct {
		Properties map[string]struct {
			Maximum int `json:"maximum"`
			Minimum int `json:"minimum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(m.AdditionalModelRequestFieldsSchema, &schema); err != nil {
		return 0, 0, false
	}
	node, present := schema.Properties["max_tokens"]
	if !present {
		return 0, 0, false
	}
	max = node.Maximum
	if max <= 0 {
		max = m.TokenLimits.MaxOutputTokens
	}
	if max <= 0 {
		return 0, 0, false
	}
	min = node.Minimum
	if min < 1 {
		min = 1
	}
	return min, max, true
}

// allEffortLevels is the full set of reasoning-effort levels, ordered low..high.
// Used when reporting per-model capabilities.
var allEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// effortConfig describes a model's reasoning-effort capability, mirroring
// Kiro's own extraction (schemaPath is "output_config" or "reasoning").
type effortConfig struct {
	SchemaPath string   // where the effort value is placed in additionalModelRequestFields
	Levels     []string // advertised levels, ordered low..high
}

// has reports whether the model advertises the given effort level.
func (e effortConfig) has(level string) bool {
	for _, l := range e.Levels {
		if l == level {
			return true
		}
	}
	return false
}

// max returns the highest advertised effort level.
func (e effortConfig) max() string {
	if len(e.Levels) == 0 {
		return ""
	}
	return e.Levels[len(e.Levels)-1]
}

// min returns the lowest advertised effort level.
func (e effortConfig) min() string {
	if len(e.Levels) == 0 {
		return ""
	}
	return e.Levels[0]
}

// clamp returns desired if the model advertises it, else the highest level.
func (e effortConfig) clamp(desired string) string {
	for _, l := range e.Levels {
		if l == desired {
			return desired
		}
	}
	return e.max()
}

// effort extracts the reasoning-effort schema from a model's
// additionalModelRequestFieldsSchema. Returns ok=false when the model does not
// support effort (e.g. "auto", claude-sonnet-4.5).
func (m kiroModelInfo) effort() (effortConfig, bool) {
	if len(m.AdditionalModelRequestFieldsSchema) == 0 {
		return effortConfig{}, false
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(m.AdditionalModelRequestFieldsSchema, &schema); err != nil {
		return effortConfig{}, false
	}
	// Kiro looks for effort under either "output_config" or "reasoning".
	for _, path := range []string{"output_config", "reasoning"} {
		if node, ok := schema.Properties[path]; ok {
			if eff, ok := node.Properties["effort"]; ok && len(eff.Enum) > 0 {
				return effortConfig{SchemaPath: path, Levels: eff.Enum}, true
			}
		}
	}
	return effortConfig{}, false
}

// ---------------------------------------------------------------------------
// Account usage / limits (GetUsageLimits).
//
// GetUsageLimits is a KiroControlPlaneBearerClient operation with the HTTP
// binding GET /getUsageLimits, served from the same management.<region>.kiro.dev
// host we already use for ListAvailableModels / ListAvailableProfiles. Kiro's
// own IDE queries it here (the AWS-native q.<region>.amazonaws.com endpoint is
// an equivalent alternative we do not need).
// ---------------------------------------------------------------------------

// kiroUsage is a parsed, page-friendly view of a GetUsageLimits response. Raw
// preserves the full upstream JSON so the admin page can surface every field.
type kiroUsage struct {
	SubscriptionTitle string           `json:"subscription_title,omitempty"`
	SubscriptionType  string           `json:"subscription_type,omitempty"`
	Email             string           `json:"email,omitempty"`
	UserID            string           `json:"user_id,omitempty"`
	NextDateReset     float64          `json:"next_date_reset,omitempty"` // Unix seconds
	ResetAt           string           `json:"reset_at,omitempty"`        // RFC3339 (from NextDateReset)
	OverageStatus     string           `json:"overage_status,omitempty"`
	Credit            *kiroCreditUsage `json:"credit,omitempty"`
	Raw               json.RawMessage  `json:"raw,omitempty"`
}

// kiroCreditUsage is the CREDIT line of usageBreakdownList, with free-trial
// allowances merged in (matching how Kiro presents the combined balance).
type kiroCreditUsage struct {
	DisplayName     string  `json:"display_name,omitempty"`
	Unit            string  `json:"unit,omitempty"`
	Currency        string  `json:"currency,omitempty"`
	Used            float64 `json:"used"`
	Limit           float64 `json:"limit"`
	Remaining       float64 `json:"remaining"`
	OverageCap      float64 `json:"overage_cap,omitempty"`
	OverageRate     float64 `json:"overage_rate,omitempty"`
	FreeTrialActive bool    `json:"free_trial_active,omitempty"`
	FreeTrialUsed   float64 `json:"free_trial_used,omitempty"`
	FreeTrialLimit  float64 `json:"free_trial_limit,omitempty"`
}

// usageLimitsWire mirrors the fields of a GetUsageLimits response we care about.
type usageLimitsWire struct {
	NextDateReset    float64 `json:"nextDateReset"`
	SubscriptionInfo struct {
		SubscriptionTitle string `json:"subscriptionTitle"`
		Type              string `json:"type"`
	} `json:"subscriptionInfo"`
	OverageConfiguration struct {
		OverageStatus string `json:"overageStatus"`
	} `json:"overageConfiguration"`
	UserInfo struct {
		Email  string `json:"email"`
		UserID string `json:"userId"`
	} `json:"userInfo"`
	UsageBreakdownList []struct {
		ResourceType              string  `json:"resourceType"`
		DisplayName               string  `json:"displayName"`
		Unit                      string  `json:"unit"`
		Currency                  string  `json:"currency"`
		CurrentUsage              float64 `json:"currentUsage"`
		CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
		UsageLimit                float64 `json:"usageLimit"`
		UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
		OverageCap                float64 `json:"overageCap"`
		OverageCapWithPrecision   float64 `json:"overageCapWithPrecision"`
		OverageRate               float64 `json:"overageRate"`
		FreeTrialInfo             *struct {
			FreeTrialStatus           string  `json:"freeTrialStatus"`
			CurrentUsage              float64 `json:"currentUsage"`
			CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
			UsageLimit                float64 `json:"usageLimit"`
			UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
		} `json:"freeTrialInfo"`
	} `json:"usageBreakdownList"`
}

// friendlySubscriptionType turns a raw subscription enum such as
// "Q_DEVELOPER_STANDALONE_PRO_PLUS" into a human label like "Pro+". Unknown
// values fall back to a title-cased form of the tier suffix.
func friendlySubscriptionType(raw string) string {
	if raw == "" {
		return ""
	}
	s := strings.ToUpper(strings.TrimSpace(raw))
	// Drop the common product prefixes, leaving the tier (FREE / PRO / PRO_PLUS / POWER).
	for _, p := range []string{"Q_DEVELOPER_STANDALONE_", "Q_DEVELOPER_", "QDEVELOPER_"} {
		s = strings.TrimPrefix(s, p)
	}
	switch s {
	case "FREE":
		return "Free"
	case "PRO":
		return "Pro"
	case "PRO_PLUS", "PROPLUS":
		return "Pro+"
	case "POWER":
		return "Power"
	case "ENTERPRISE":
		return "Enterprise"
	}
	// Fallback: "SOME_TIER" -> "Some Tier".
	return titleCaseWords(strings.ReplaceAll(s, "_", " "))
}

// friendlyUnit turns a raw usage unit enum into a readable label.
func friendlyUnit(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "":
		return ""
	case "INVOCATIONS", "INVOCATION":
		return "次调用"
	case "CREDIT", "CREDITS":
		return "credits"
	case "TOKENS", "TOKEN":
		return "tokens"
	default:
		return titleCaseWords(strings.ToLower(strings.ReplaceAll(raw, "_", " ")))
	}
}

// titleCaseWords upper-cases the first letter of each space-separated word.
func titleCaseWords(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
	}
	return strings.Join(parts, " ")
}

// pick returns the precise value when present, else the integer fallback.
func pick(precise, fallback float64) float64 {
	if precise != 0 {
		return precise
	}
	return fallback
}

// parseKiroUsage turns a raw GetUsageLimits body into a kiroUsage. The CREDIT
// breakdown line drives the balance; an ACTIVE free trial is merged into the
// totals. Raw is retained verbatim for full display.
func parseKiroUsage(raw []byte) (*kiroUsage, error) {
	var w usageLimitsWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}

	u := &kiroUsage{
		SubscriptionTitle: w.SubscriptionInfo.SubscriptionTitle,
		SubscriptionType:  friendlySubscriptionType(w.SubscriptionInfo.Type),
		Email:             w.UserInfo.Email,
		UserID:            w.UserInfo.UserID,
		NextDateReset:     w.NextDateReset,
		OverageStatus:     w.OverageConfiguration.OverageStatus,
		Raw:               json.RawMessage(raw),
	}
	if w.NextDateReset > 0 {
		u.ResetAt = time.Unix(int64(w.NextDateReset), 0).UTC().Format(time.RFC3339)
	}

	for _, b := range w.UsageBreakdownList {
		if b.ResourceType != "CREDIT" && b.DisplayName != "Credit" && b.DisplayName != "Credits" {
			continue
		}
		c := &kiroCreditUsage{
			DisplayName: b.DisplayName,
			Unit:        friendlyUnit(b.Unit),
			Currency:    b.Currency,
			Used:        pick(b.CurrentUsageWithPrecision, b.CurrentUsage),
			Limit:       pick(b.UsageLimitWithPrecision, b.UsageLimit),
			OverageCap:  pick(b.OverageCapWithPrecision, b.OverageCap),
			OverageRate: b.OverageRate,
		}
		if ft := b.FreeTrialInfo; ft != nil && ft.FreeTrialStatus == "ACTIVE" {
			c.FreeTrialActive = true
			c.FreeTrialUsed = pick(ft.CurrentUsageWithPrecision, ft.CurrentUsage)
			c.FreeTrialLimit = pick(ft.UsageLimitWithPrecision, ft.UsageLimit)
			c.Used += c.FreeTrialUsed
			c.Limit += c.FreeTrialLimit
		}
		c.Remaining = c.Limit - c.Used
		if c.Remaining < 0 {
			c.Remaining = 0
		}
		u.Credit = c
		break
	}
	return u, nil
}

// GetUsage fetches the account's usage/limits from the control plane. On a
// 401/403 the token is force-refreshed and the request retried once.
func (c *KiroClient) GetUsage(ctx context.Context, creds kiroCredentials) (*kiroUsage, error) {
	raw, status, err := c.getUsageOnce(ctx, creds)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if rerr := creds.refresh(ctx); rerr == nil {
			raw, status, err = c.getUsageOnce(ctx, creds)
			if err != nil {
				return nil, err
			}
		}
	}
	if status < 200 || status >= 300 {
		return nil, &kiroHTTPError{Status: status, Body: string(raw)}
	}
	return parseKiroUsage(raw)
}

// getUsageOnce performs a single GET /getUsageLimits attempt, returning the body
// and HTTP status.
func (c *KiroClient) getUsageOnce(ctx context.Context, creds kiroCredentials) ([]byte, int, error) {
	token, err := creds.accessToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	arn, _ := creds.profileArn(ctx) // best effort; some tiers don't need it

	endpoint := fmt.Sprintf("https://management.%s.kiro.dev/getUsageLimits", creds.apiRegion())
	q := url.Values{
		"origin":          {"AI_EDITOR"},
		"resourceType":    {"AGENTIC_REQUEST"},
		"isEmailRequired": {"true"},
	}
	if arn != "" {
		q.Set("profileArn", arn)
	}
	reqURL := endpoint + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "kiro-anthropic/"+version)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// ListModels queries the Kiro control plane for the model IDs available to this
// account (KiroControlPlaneBearerService.ListAvailableModels).
func (c *KiroClient) ListModels(ctx context.Context, creds kiroCredentials) ([]kiroModelInfo, error) {
	token, err := creds.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	arn, _ := creds.profileArn(ctx) // best effort; some tiers don't need it

	reqBody := map[string]any{"origin": "AI_EDITOR"}
	if arn != "" {
		reqBody["profileArn"] = arn
	}
	payload, _ := json.Marshal(reqBody)

	endpoint := fmt.Sprintf("https://management.%s.kiro.dev/", creds.apiRegion())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "KiroControlPlaneBearerService.ListAvailableModels")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &kiroHTTPError{Status: resp.StatusCode, Body: readSnippet(resp.Body)}
	}

	var out struct {
		Models []kiroModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	return out.Models, nil
}

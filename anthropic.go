package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Anthropic Messages API request/response types.
// Only the fields we translate are modelled; the rest are ignored.
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"` // string or []block
	Tools         []anthropicTool    `json:"tools,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`

	// Reasoning-effort control (native Anthropic field + OpenAI-style alias).
	OutputConfig    *anthropicOutputConfig `json:"output_config,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"`

	// Extended-thinking toggle (Anthropic native). type is "enabled"|"disabled".
	Thinking *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`                    // "enabled" | "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // accepted; Kiro maps effort, not token budgets
}

// effortMinimize is a sentinel returned by requestedEffort meaning "use the
// model's lowest effort level" (the client asked to disable extended thinking,
// which Kiro cannot fully turn off for reasoning models).
const effortMinimize = "\x00min"

// thinkingDisabled reports whether the client explicitly turned extended
// thinking off (thinking.type == "disabled"). It is the single condition behind
// both effort minimization and response-side suppression.
func thinkingDisabled(areq *anthropicRequest) bool {
	return areq.Thinking != nil && strings.EqualFold(areq.Thinking.Type, "disabled")
}

// requestedEffort returns the effort level the client asked for, if any.
// Precedence: output_config.effort (native) > reasoning_effort (alias) >
// thinking toggle. An explicit thinking.type=="disabled" minimizes effort;
// "enabled" leaves the effort at its default (top-out) unless set above.
func requestedEffort(areq *anthropicRequest) string {
	if areq.OutputConfig != nil && areq.OutputConfig.Effort != "" {
		return areq.OutputConfig.Effort
	}
	if areq.ReasoningEffort != "" {
		return areq.ReasoningEffort
	}
	if thinkingDisabled(areq) {
		return effortMinimize
	}
	return ""
}

// thinkingSuppressed reports whether reasoning content should be dropped from
// the response (matching Anthropic's contract that no thinking blocks appear
// when thinking is disabled).
func thinkingSuppressed(areq *anthropicRequest) bool {
	return thinkingDisabled(areq)
}

type anthropicMessage struct {
	Role    string          `json:"role"`    // "user" | "assistant"
	Content json.RawMessage `json:"content"` // string or []contentBlock
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// anthropicContentBlock covers every block type we read from a request.
type anthropicContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// redacted_thinking
	Data string `json:"data,omitempty"`

	Source *anthropicImageSource `json:"source,omitempty"`

	// document
	Title string `json:"title,omitempty"` // display name for document blocks

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []block
	IsError   bool            `json:"is_error,omitempty"`

	// cache_control (prompt-caching breakpoint) is retained verbatim so per-block
	// transforms that re-serialize a message do not silently drop it.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`                 // "base64" | "url"
	MediaType string `json:"media_type,omitempty"` // "image/png", ...
	Data      string `json:"data,omitempty"`       // base64 bytes (for type=base64)
	URL       string `json:"url,omitempty"`        // for type=url
}

// mediaTypeToKiroImageFormat maps an Anthropic media_type to a Kiro image format.
func mediaTypeToKiroImageFormat(mt string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "image/png":
		return "png", true
	case "image/jpeg", "image/jpg":
		return "jpeg", true
	case "image/gif":
		return "gif", true
	case "image/webp":
		return "webp", true
	}
	return "", false
}

// convertImage maps an Anthropic image block to a Kiro image. Only base64
// sources with a supported media type are handled (url sources are skipped).
func convertImage(b anthropicContentBlock) (kiroImage, bool) {
	if b.Source == nil || b.Source.Data == "" {
		return kiroImage{}, false
	}
	format, ok := mediaTypeToKiroImageFormat(b.Source.MediaType)
	if !ok {
		return kiroImage{}, false
	}
	return kiroImage{Format: format, Source: kiroImageSource{Bytes: b.Source.Data}}, true
}

// documentMediaTypeToKiroFormat maps an Anthropic document media_type to a
// kiro-cli DocumentBlock format enum (csv | doc | md | pdf | txt | xls).
// Live-verified against runtime.us-east-1.kiro.dev: the documents member and
// its {name, format, source:{bytes}} shape are accepted (2026-09).
func documentMediaTypeToKiroFormat(mt string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "application/pdf":
		return "pdf", true
	case "text/plain":
		return "txt", true
	case "text/csv":
		return "csv", true
	case "text/markdown":
		return "md", true
	case "application/msword":
		return "doc", true
	case "application/vnd.ms-excel":
		return "xls", true
	}
	return "", false
}

// convertDocument maps an Anthropic document block to a kiro-cli-style
// DocumentBlock. Only base64 sources with a supported media type are handled
// (Anthropic "text" sources are inlined by re-encoding; url sources are
// skipped).
func convertDocument(b anthropicContentBlock) (kiroDocument, bool) {
	if b.Source == nil || b.Source.Data == "" {
		return kiroDocument{}, false
	}
	format, ok := documentMediaTypeToKiroFormat(b.Source.MediaType)
	if !ok {
		return kiroDocument{}, false
	}
	name := b.Title
	if name == "" {
		name = "document"
	}
	data := b.Source.Data
	if strings.EqualFold(b.Source.Type, "text") {
		// Anthropic text documents carry raw text; the Kiro DocumentBlock
		// source is always base64 bytes, so re-encode.
		data = base64.StdEncoding.EncodeToString([]byte(b.Source.Data))
	}
	return kiroDocument{Name: name, Format: format, Source: kiroDocumentSource{Bytes: data}}, true
}

// ---------------------------------------------------------------------------
// Response types (non-streaming).
// ---------------------------------------------------------------------------

type anthropicResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"` // "message"
	Role         string               `json:"role"` // "assistant"
	Model        string               `json:"model"`
	Content      []anthropicRespBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        anthropicUsage       `json:"usage"`
}

type anthropicRespBlock struct {
	Type string `json:"type"` // "text" | "thinking" | "redacted_thinking" | "tool_use"
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// redacted_thinking
	Data string `json:"data,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---------------------------------------------------------------------------
// Model mapping.
// ---------------------------------------------------------------------------

// knownKiroModels are IDs that pass straight through to Kiro. This is the set
// advertised by ListAvailableModels; unknown IDs fall back to keyword mapping.
var knownKiroModels = map[string]bool{
	"auto":              true,
	"claude-sonnet-5":   true,
	"claude-opus-4.8":   true,
	"claude-opus-4.7":   true,
	"claude-opus-4.6":   true,
	"claude-opus-4.5":   true,
	"claude-sonnet-4.6": true,
	"claude-sonnet-4.5": true,
	"claude-sonnet-4":   true,
	"claude-haiku-4.5":  true,
	"deepseek-3.2":      true,
	"minimax-m2.5":      true,
	"minimax-m2.1":      true,
	"glm-5":             true,
	"qwen3-coder-next":  true,
}

// mapModel translates an Anthropic model name to a Kiro modelId.
func mapModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "auto"
	}
	if knownKiroModels[model] {
		return model
	}
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "opus"):
		return "claude-opus-4.8"
	case strings.Contains(lower, "haiku"):
		return "claude-haiku-4.5"
	case strings.Contains(lower, "sonnet"):
		return "claude-sonnet-4.5"
	case lower == "default" || lower == "auto":
		return "auto"
	}
	// A dotted, Kiro-style id we don't explicitly know: pass through.
	if strings.Contains(model, ".") {
		return model
	}
	return "auto"
}

// resolveModel maps a client-supplied model name to a concrete modelId from the
// given account's available models, returning the matched model. ok is false
// when no model on this account matches, so the caller skips the account rather
// than sending a request the runtime would reject as INVALID_MODEL_ID. It is a
// pure function over the cached list (no I/O).
//
// Unlike mapModel it never passes an unknown id through: an unmatched name
// yields ok=false instead of a guess. mapModel stays as a last-resort fallback
// when an account's list cannot be fetched.
func resolveModel(clientModel string, models []kiroModelInfo) (string, kiroModelInfo, bool) {
	if len(models) == 0 {
		return "", kiroModelInfo{}, false
	}
	trimmed := strings.TrimSpace(clientModel)
	q := strings.ToLower(trimmed)

	// auto/default/empty: the account's flagged default, else an active model
	// (avoiding LEGACY), else the first model.
	if q == "" || q == "auto" || q == "default" {
		for _, m := range models {
			if m.Default || m.IsDefault {
				return m.ModelID, m, true
			}
		}
		if a := preferActive(models); len(a) > 0 {
			return a[0].ModelID, a[0], true
		}
		return models[0].ModelID, models[0], true
	}

	// Exact id, then exact display name (case-insensitive, whitespace-trimmed).
	for _, m := range models {
		if strings.EqualFold(m.ModelID, trimmed) {
			return m.ModelID, m, true
		}
	}
	for _, m := range models {
		if strings.EqualFold(m.ModelName, trimmed) {
			return m.ModelID, m, true
		}
	}

	// Family keyword (carrying tier, if any): best-effort for vague names like
	// "claude-3-5-sonnet-20241022" or "gpt-4o". Picks the highest version.
	if id, m, ok := matchByFamily(q, models); ok {
		return id, m, true
	}

	return "", kiroModelInfo{}, false
}

// modelFamilies maps a family keyword found in the query to a predicate over a
// candidate model id. The order matters only for the claude family, which also
// recognizes the opus/sonnet/haiku tiers.
func modelFamily(q string) (family, tier string) {
	switch {
	case strings.Contains(q, "claude") || strings.Contains(q, "opus") ||
		strings.Contains(q, "sonnet") || strings.Contains(q, "haiku"):
		family = "claude"
	case strings.Contains(q, "gpt"):
		family = "gpt"
	case strings.Contains(q, "glm"):
		family = "glm"
	case strings.Contains(q, "deepseek"):
		family = "deepseek"
	case strings.Contains(q, "minimax"):
		family = "minimax"
	case strings.Contains(q, "qwen"):
		family = "qwen"
	}
	for _, t := range []string{"opus", "sonnet", "haiku"} {
		if strings.Contains(q, t) {
			tier = t
			break
		}
	}
	return family, tier
}

// matchByFamily narrows models to the same family (and tier, if specified) as
// the query and returns the highest-version match.
func matchByFamily(q string, models []kiroModelInfo) (string, kiroModelInfo, bool) {
	family, tier := modelFamily(q)
	if family == "" {
		return "", kiroModelInfo{}, false
	}
	var cands []kiroModelInfo
	for _, m := range models {
		id := strings.ToLower(m.ModelID)
		if !strings.Contains(id, family) {
			continue
		}
		if tier != "" && !strings.Contains(id, tier) {
			continue
		}
		cands = append(cands, m)
	}
	cands = preferActive(cands)
	if len(cands) == 0 {
		return "", kiroModelInfo{}, false
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if cmpVersion(c.ModelID, best.ModelID) > 0 {
			best = c
		}
	}
	return best.ModelID, best, true
}

// cmpVersion reports whether model id a is newer/preferred over b: first by
// trailing numeric version segments, then a flagship-tier preference (so a
// generic family query maps to opus rather than a lexicographic sibling when
// versions tie), then the id itself.
func cmpVersion(a, b string) int {
	if c := compareIntSlices(versionRank(a), versionRank(b)); c != 0 {
		return c
	}
	if c := tierPref(a) - tierPref(b); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// tierPref ranks flagship Claude tiers so a family query without a tier prefers
// opus over sonnet over haiku when versions tie. Other ids score 0.
func tierPref(id string) int {
	id = strings.ToLower(id)
	switch {
	case strings.Contains(id, "opus"):
		return 3
	case strings.Contains(id, "sonnet"):
		return 2
	case strings.Contains(id, "haiku"):
		return 1
	}
	return 0
}

// preferActive drops LEGACY/retired models when any active ones are present, so
// a retired higher-version model can't win and trigger INVALID_MODEL_ID.
func preferActive(cands []kiroModelInfo) []kiroModelInfo {
	var active []kiroModelInfo
	for _, c := range cands {
		if !strings.EqualFold(c.Status, "LEGACY") {
			active = append(active, c)
		}
	}
	if len(active) > 0 {
		return active
	}
	return cands
}

// versionRank extracts the numeric segments of an id in order
// ("claude-opus-4.8" -> [4,8], "minimax-m2.5" -> [2,5], "qwen3-coder-next" -> [3]).
func versionRank(id string) []int {
	var ranks []int
	num, any := 0, false
	for _, r := range id {
		if r >= '0' && r <= '9' {
			num = num*10 + int(r-'0')
			any = true
			continue
		}
		if any {
			ranks = append(ranks, num)
			num, any = 0, false
		}
	}
	if any {
		ranks = append(ranks, num)
	}
	return ranks
}

func compareIntSlices(a, b []int) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Request translation: Anthropic -> Kiro.
// ---------------------------------------------------------------------------

// buildKiroRequest converts an Anthropic Messages request into the Kiro
// GenerateAssistantResponse payload, resolving the model via the static mapModel
// fallback and assigning a fresh conversation UUID.
func buildKiroRequest(cfg *Config, areq *anthropicRequest) (*kiroRequest, error) {
	return buildKiroRequestWithModelAndConversationID(cfg, areq, mapModel(areq.Model), uuid.NewString())
}

// buildKiroRequestWithModelAndConversationID stamps the concrete Kiro modelId on
// every user turn and preserves the caller-supplied conversation ID. Assistant
// turns carry no modelId.
func buildKiroRequestWithModelAndConversationID(cfg *Config, areq *anthropicRequest, modelID, conversationID string) (*kiroRequest, error) {
	if len(areq.Messages) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}

	system := extractText(areq.System)

	// Convert every Anthropic message to a Kiro message, preserving order.
	kmsgs := make([]kiroMessage, 0, len(areq.Messages))
	for _, m := range areq.Messages {
		km, err := convertMessage(m, modelID)
		if err != nil {
			return nil, err
		}
		kmsgs = append(kmsgs, km)
	}

	// The current message must be a userInputMessage. In the common case the
	// last message is from the user. If it is an assistant turn (prefill), we
	// push it into history and continue with a minimal user turn.
	var current kiroMessage
	var history []kiroMessage
	last := kmsgs[len(kmsgs)-1]
	if last.UserInputMessage != nil {
		current = last
		history = kmsgs[:len(kmsgs)-1]
	} else {
		current = kiroMessage{UserInputMessage: &kiroUserInputMessage{Content: " ", ModelID: modelID, Origin: "AI_EDITOR"}}
		history = kmsgs
	}

	// Attach tools to the current user message.
	if len(areq.Tools) > 0 {
		if current.UserInputMessage.UserInputMessageContext == nil {
			current.UserInputMessage.UserInputMessageContext = &kiroUserInputMessageContext{}
		}
		for _, t := range areq.Tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			current.UserInputMessage.UserInputMessageContext.Tools = append(
				current.UserInputMessage.UserInputMessageContext.Tools,
				kiroTool{ToolSpecification: kiroToolSpecification{
					Name:        shortenToolName(t.Name),
					Description: t.Description,
					InputSchema: kiroInputSchema{JSON: schema},
				}},
			)
		}
	}

	history = sanitizeHistory(history)

	// The Kiro runtime rejects a top-level systemPrompt field (HTTP 400
	// "Improperly formed request"), so the Anthropic "system" is injected as a
	// leading user/assistant turn pair in history instead. The runtime honors
	// instructions delivered this way while rejecting the dedicated field.
	if system != "" {
		preamble := []kiroMessage{
			{UserInputMessage: &kiroUserInputMessage{
				Content: system, ModelID: modelID, Origin: "AI_EDITOR",
			}},
			{AssistantResponseMessage: &kiroAssistantMessage{
				Content: "Understood. I will follow these instructions for this conversation.",
			}},
		}
		history = append(preamble, history...)
	}

	return &kiroRequest{
		ConversationState: kiroConversationState{
			ChatTriggerType: "MANUAL",
			ConversationID:  conversationID,
			CurrentMessage:  current,
			History:         history,
		},
		ProfileArn: "", // filled in by the server after resolution
		AgentMode:  cfg.AgentMode,
	}, nil
}

// convertMessage maps one Anthropic message to a Kiro message.
func convertMessage(m anthropicMessage, modelID string) (kiroMessage, error) {
	blocks, err := parseContentBlocks(m.Content)
	if err != nil {
		return kiroMessage{}, err
	}

	if m.Role == "assistant" {
		am := &kiroAssistantMessage{}
		var text strings.Builder
		var hasCacheBreakpoint bool
		for _, b := range blocks {
			if b.CacheControl != nil {
				hasCacheBreakpoint = true
			}
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "thinking":
				// Round-trip extended thinking (with its signature) so the
				// backend can validate the reasoning chain on the next turn.
				// The ReasoningContent union holds one member; first wins.
				if am.ReasoningContent == nil && (b.Thinking != "" || b.Signature != "") {
					am.ReasoningContent = &kiroReasoningContent{
						ReasoningText: &kiroReasoningText{Text: b.Thinking, Signature: b.Signature},
					}
				}
			case "redacted_thinking":
				if am.ReasoningContent == nil && b.Data != "" {
					am.ReasoningContent = &kiroReasoningContent{RedactedContent: b.Data}
				}
			case "tool_use":
				// The Kiro runtime requires toolUse.input to be a JSON object;
				// a null or non-object value is rejected as "Invalid tool use
				// format". Coerce to {} unless it is already an object.
				am.ToolUses = append(am.ToolUses, kiroToolUse{
					ToolUseID: b.ID,
					Name:      shortenToolName(b.Name),
					Input:     objectToolInput(b.Input),
				})
			}
		}
		if hasCacheBreakpoint {
			am.CachePoint = &kiroCachePoint{Type: "default"}
		}
		am.Content = text.String()
		// An assistant turn carrying tool uses is well-formed with empty
		// content; only a degenerate turn with neither text nor tool uses
		// needs a placeholder so the message is not empty.
		if am.Content == "" && len(am.ToolUses) == 0 {
			am.Content = " "
		}
		return kiroMessage{AssistantResponseMessage: am}, nil
	}

	// role == "user" (default)
	um := &kiroUserInputMessage{ModelID: modelID, Origin: "AI_EDITOR"}
	var text strings.Builder
	var toolResults []kiroToolResult
	var images []kiroImage
	var documents []kiroDocument
	var hasCacheBreakpoint bool
	for _, b := range blocks {
		if b.CacheControl != nil {
			hasCacheBreakpoint = true
		}
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_result":
			toolResults = append(toolResults, convertToolResult(b))
		case "image":
			if img, ok := convertImage(b); ok {
				images = append(images, img)
			} else {
				// Unsupported source (e.g. url) — note it so the model knows.
				text.WriteString("\n[unsupported image omitted]\n")
			}
		case "document":
			if doc, ok := convertDocument(b); ok {
				documents = append(documents, doc)
			} else {
				// Unsupported source or media type — note it so the model knows.
				text.WriteString("\n[unsupported document omitted]\n")
			}
		}
	}
	um.Content = text.String()
	if len(images) > 0 {
		um.Images = images
	}
	if len(documents) > 0 {
		um.Documents = documents
	}
	if hasCacheBreakpoint {
		um.CachePoint = &kiroCachePoint{Type: "default"}
	}
	if len(toolResults) > 0 {
		um.UserInputMessageContext = &kiroUserInputMessageContext{ToolResults: toolResults}
		// A tool-result turn often has no text; CodeWhisperer tolerates empty
		// content here as long as toolResults are present.
	}
	if um.Content == "" && len(toolResults) == 0 && len(images) == 0 && len(documents) == 0 {
		um.Content = " "
	}
	return kiroMessage{UserInputMessage: um}, nil
}

// convertToolResult maps an Anthropic tool_result block to a Kiro tool result.
func convertToolResult(b anthropicContentBlock) kiroToolResult {
	status := "success"
	if b.IsError {
		status = "error"
	}
	tr := kiroToolResult{ToolUseID: b.ToolUseID, Status: status}

	// tool_result content may be a string or an array of blocks.
	if len(b.Content) == 0 {
		tr.Content = []kiroToolResultContent{{Text: ""}}
		return tr
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		tr.Content = []kiroToolResultContent{{Text: s}}
		return tr
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		for _, cb := range blocks {
			switch cb.Type {
			case "text":
				tr.Content = append(tr.Content, kiroToolResultContent{Text: cb.Text})
			default:
				// Represent non-text result blocks as JSON so nothing is lost.
				raw, _ := json.Marshal(cb)
				tr.Content = append(tr.Content, kiroToolResultContent{JSON: raw})
			}
		}
		if len(tr.Content) == 0 {
			tr.Content = []kiroToolResultContent{{Text: ""}}
		}
		return tr
	}
	// Fallback: store the raw JSON.
	tr.Content = []kiroToolResultContent{{JSON: b.Content}}
	return tr
}

// stripReasoningFromHistory removes reasoningContent from every assistant turn
// in the request history, reporting whether anything was removed. Mirrors the
// Kiro client's own recovery: when the backend rejects a stale or invalid
// thinking signature (THINKING_SIGNATURE_INVALID), the request is retried once
// with the reasoning stripped.
func stripReasoningFromHistory(kreq *kiroRequest) bool {
	stripped := false
	for i := range kreq.ConversationState.History {
		am := kreq.ConversationState.History[i].AssistantResponseMessage
		if am != nil && am.ReasoningContent != nil {
			am.ReasoningContent = nil
			stripped = true
		}
	}
	return stripped
}

// hasReasoningInHistory reports whether any assistant history turn carries
// reasoning content. It lets request recovery preserve a prior signature fix
// when a trimmed Anthropic request is translated again.
func hasReasoningInHistory(kreq *kiroRequest) bool {
	for i := range kreq.ConversationState.History {
		am := kreq.ConversationState.History[i].AssistantResponseMessage
		if am != nil && am.ReasoningContent != nil {
			return true
		}
	}
	return false
}

// trimOldestCompleteTurn removes the oldest closed conversation unit while
// preserving the current turn. A tool-calling unit includes every
// assistant(tool_use) -> user(tool_result) exchange through the first terminal
// assistant response. Malformed or still-active chains are left untouched.
func trimOldestCompleteTurn(areq *anthropicRequest) bool {
	end, ok := oldestCompleteTurnEnd(areq.Messages)
	if !ok {
		return false
	}
	areq.Messages = areq.Messages[end:]
	return true
}

// oldestCompleteTurnEnd returns the exclusive end of the oldest removable
// conversation unit. At least one later message must remain so an assistant
// prefill or the current user turn is never discarded. Consecutive same-role
// messages (allowed by the Anthropic API) are treated as a single logical turn.
func oldestCompleteTurnEnd(messages []anthropicMessage) (int, bool) {
	if len(messages) < 3 {
		return 0, false
	}

	end, uses, results, ok := logicalTurnEnd(messages, 0)
	if !ok || messages[0].Role != "user" || len(uses) != 0 || len(results) != 0 {
		// A leading tool result belongs to a tool use outside the removable
		// history, so its chain cannot be proven closed.
		return 0, false
	}

	for i := end; ; {
		if i >= len(messages) {
			return 0, false
		}
		end, uses, results, ok = logicalTurnEnd(messages, i)
		if !ok || messages[i].Role != "assistant" || len(results) != 0 {
			return 0, false
		}
		i = end

		if len(uses) == 0 {
			// Terminal assistant with no tool calls. The next turn must be
			// a fresh user turn (no tool results) that begins a new unit.
			if i >= len(messages) {
				return 0, false
			}
			_, nextUses, nextResults, valid := logicalTurnEnd(messages, i)
			if !valid || messages[i].Role != "user" || len(nextUses) != 0 || len(nextResults) != 0 {
				return 0, false
			}
			return i, true
		}

		// Assistant has tool use(s). The next turn must be a user with
		// matching tool results.
		if i >= len(messages) {
			return 0, false
		}
		end, resultUses, toolResults, valid := logicalTurnEnd(messages, i)
		if !valid || messages[i].Role != "user" || len(resultUses) != 0 || !sameToolIDs(uses, toolResults) {
			return 0, false
		}
		i = end
	}
}

// logicalTurnEnd scans consecutive same-role messages starting at start,
// returning the exclusive end index and the union of all tool_use / tool_result
// IDs across the merged turn. Empty or duplicate IDs make the turn invalid.
func logicalTurnEnd(messages []anthropicMessage, start int) (end int, uses, results map[string]struct{}, valid bool) {
	if start >= len(messages) {
		return start, nil, nil, true
	}
	role := messages[start].Role
	uses = make(map[string]struct{})
	results = make(map[string]struct{})
	for i := start; i < len(messages); i++ {
		if messages[i].Role != role {
			return i, uses, results, true
		}
		u, r, ok := messageToolIDs(messages[i])
		if !ok {
			return 0, nil, nil, false
		}
		for id := range u {
			if _, exists := uses[id]; exists {
				return 0, nil, nil, false // duplicate across merged messages
			}
			uses[id] = struct{}{}
		}
		for id := range r {
			if _, exists := results[id]; exists {
				return 0, nil, nil, false
			}
			results[id] = struct{}{}
		}
	}
	return len(messages), uses, results, true
}

// messageToolIDs extracts and validates tool IDs from one message. Duplicate or
// empty IDs make the message unsafe to use as a trimming boundary.
func messageToolIDs(message anthropicMessage) (uses, results map[string]struct{}, valid bool) {
	blocks, err := parseContentBlocks(message.Content)
	if err != nil {
		return nil, nil, false
	}
	uses = make(map[string]struct{})
	results = make(map[string]struct{})
	for _, block := range blocks {
		var id string
		var ids map[string]struct{}
		switch block.Type {
		case "tool_use":
			id, ids = block.ID, uses
		case "tool_result":
			id, ids = block.ToolUseID, results
		default:
			continue
		}
		if id == "" {
			return nil, nil, false
		}
		if _, exists := ids[id]; exists {
			return nil, nil, false
		}
		ids[id] = struct{}{}
	}
	return uses, results, true
}

func sameToolIDs(want, got map[string]struct{}) bool {
	if len(want) != len(got) {
		return false
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			return false
		}
	}
	return true
}

// sanitizeHistory drops leading assistant turns and collapses so history begins
// with a user turn, which the CodeWhisperer API expects.
func sanitizeHistory(history []kiroMessage) []kiroMessage {
	start := 0
	for start < len(history) && history[start].UserInputMessage == nil {
		start++
	}
	if start > 0 {
		history = history[start:]
	}
	if len(history) == 0 {
		return nil
	}
	return history
}

// ---------------------------------------------------------------------------
// Content parsing helpers (polymorphic string | []block).
// ---------------------------------------------------------------------------

// parseContentBlocks turns a message "content" (string or array) into blocks.
func parseContentBlocks(raw json.RawMessage) ([]anthropicContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []anthropicContentBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("invalid message content: %w", err)
	}
	return blocks, nil
}

// extractText flattens a "system" field (string or []{type:text,text}) to text.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ---------------------------------------------------------------------------
// Response assembly: Kiro events -> Anthropic content blocks / SSE.
// ---------------------------------------------------------------------------

// emitFunc sends one SSE event. When nil, the assembler only accumulates
// (non-streaming mode).
type emitFunc func(event string, data any) error

// blockAssembler turns the flat stream of Kiro events into ordered Anthropic
// content blocks, optionally emitting streaming SSE deltas as it goes.
type blockAssembler struct {
	emit emitFunc

	// emitThinking controls whether reasoning is surfaced as thinking blocks.
	// Defaults to true; the server disables it when the client turned thinking
	// off (thinking.type == "disabled").
	emitThinking bool

	index    int
	openKind string // "", "text", "thinking", "redacted_thinking", "tool_use"

	textBuf      strings.Builder
	thinkingBuf  strings.Builder
	thinkingSig  strings.Builder
	redactedBuf  strings.Builder
	toolID       string
	toolName     string
	toolInputBuf strings.Builder

	// toolNameMap restores original tool names on tool_use responses when the
	// request side shortened an over-long name (shortened -> original). Nil when
	// no tool name needed shortening.
	toolNameMap map[string]string

	blocks     []anthropicRespBlock
	sawToolUse bool

	// finalStopReason is the Anthropic stop_reason derived from the backend's
	// authoritative metadataEvent.stopReason; wins over the sawToolUse guess
	// when present. Empty until setStopReason is called.
	finalStopReason string

	// leakCarry buffers trailing text that might be (the start of) a stray
	// tool-call opening marker, held across frames until it can be decided.
	// See the leak-filter section below.
	leakCarry string
}

func newBlockAssembler(emit emitFunc, toolNameMap map[string]string) *blockAssembler {
	return &blockAssembler{emit: emit, emitThinking: true, toolNameMap: toolNameMap}
}

// addReasoning folds a Kiro reasoningContentEvent into a thinking (or
// redacted_thinking) content block. Text streams as thinking_delta, the
// signature as signature_delta; redacted reasoning is buffered and emitted
// whole on close (Anthropic defines no delta event for it).
func (a *blockAssembler) addReasoning(ev *kiroEvent) error {
	if !a.emitThinking {
		return nil // client disabled thinking: drop reasoning entirely
	}
	if err := a.flushLeakCarry(); err != nil {
		return err
	}

	// Redacted reasoning is a distinct block type carrying opaque data.
	if ev.ReasoningRedacted != "" {
		if a.openKind != "redacted_thinking" {
			if err := a.closeOpen(); err != nil {
				return err
			}
			a.openKind = "redacted_thinking"
			a.redactedBuf.Reset()
		}
		a.redactedBuf.WriteString(ev.ReasoningRedacted)
		return nil
	}

	if ev.ReasoningText == "" && ev.ReasoningSignature == "" {
		return nil
	}

	if a.openKind != "thinking" {
		if err := a.closeOpen(); err != nil {
			return err
		}
		a.openKind = "thinking"
		a.thinkingBuf.Reset()
		a.thinkingSig.Reset()
		if err := a.send("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         a.index,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}); err != nil {
			return err
		}
	}

	if ev.ReasoningText != "" {
		a.thinkingBuf.WriteString(ev.ReasoningText)
		if err := a.send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.index,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.ReasoningText},
		}); err != nil {
			return err
		}
	}
	if ev.ReasoningSignature != "" {
		a.thinkingSig.WriteString(ev.ReasoningSignature)
		if err := a.send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.index,
			"delta": map[string]any{"type": "signature_delta", "signature": ev.ReasoningSignature},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *blockAssembler) addText(text string) error {
	if text == "" {
		return nil
	}
	if a.openKind != "text" {
		if err := a.closeOpen(); err != nil {
			return err
		}
		a.openKind = "text"
		a.textBuf.Reset()
		if err := a.send("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         a.index,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
	}
	a.textBuf.WriteString(text)
	return a.send("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": a.index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (a *blockAssembler) addToolUse(ev *kiroEvent) error {
	if err := a.flushLeakCarry(); err != nil {
		return err
	}
	newTool := ev.ToolUseID != "" && (a.openKind != "tool_use" || ev.ToolUseID != a.toolID)
	if newTool {
		if err := a.closeOpen(); err != nil {
			return err
		}
		a.openKind = "tool_use"
		a.toolID = ev.ToolUseID
		a.toolName = ev.ToolName
		// Restore the original tool name if the request side shortened it.
		if a.toolNameMap != nil {
			if orig, ok := a.toolNameMap[a.toolName]; ok {
				a.toolName = orig
			}
		}
		a.toolInputBuf.Reset()
		a.sawToolUse = true
		if err := a.send("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": a.index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    a.toolID,
				"name":  a.toolName,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
	} else if a.openKind != "tool_use" {
		// A tool chunk with no id and nothing open: ignore.
		return nil
	}

	if ev.ToolInput != "" {
		a.toolInputBuf.WriteString(ev.ToolInput)
		if err := a.send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolInput},
		}); err != nil {
			return err
		}
	}
	if ev.ToolStop {
		return a.closeOpen()
	}
	return nil
}

// closeOpen finalizes the currently open block (if any).
func (a *blockAssembler) closeOpen() error {
	switch a.openKind {
	case "text":
		a.blocks = append(a.blocks, anthropicRespBlock{Type: "text", Text: a.textBuf.String()})
	case "thinking":
		a.blocks = append(a.blocks, anthropicRespBlock{
			Type:      "thinking",
			Thinking:  a.thinkingBuf.String(),
			Signature: a.thinkingSig.String(),
		})
	case "redacted_thinking":
		// No incremental deltas exist for redacted reasoning: emit the whole
		// block now (start carries the full data), then fall through to stop.
		data := a.redactedBuf.String()
		if err := a.send("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         a.index,
			"content_block": map[string]any{"type": "redacted_thinking", "data": data},
		}); err != nil {
			return err
		}
		a.blocks = append(a.blocks, anthropicRespBlock{Type: "redacted_thinking", Data: data})
	case "tool_use":
		a.blocks = append(a.blocks, anthropicRespBlock{
			Type:  "tool_use",
			ID:    a.toolID,
			Name:  a.toolName,
			Input: normalizeToolInput(a.toolInputBuf.String()),
		})
	default:
		return nil
	}
	if err := a.send("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": a.index,
	}); err != nil {
		return err
	}
	a.index++
	a.openKind = ""
	return nil
}

func (a *blockAssembler) send(event string, data any) error {
	if a.emit == nil {
		return nil
	}
	return a.emit(event, data)
}

// stopReason reports the Anthropic stop_reason for the assembled message.
// The backend's authoritative metadataEvent.stopReason wins when present;
// otherwise fall back to inferring tool_use from the emitted blocks.
func (a *blockAssembler) stopReason() string {
	if a.finalStopReason != "" {
		return a.finalStopReason
	}
	if a.sawToolUse {
		return "tool_use"
	}
	return "end_turn"
}

// setStopReason records the stop reason carried by a metadataEvent, mapped to
// Anthropic's vocabulary. Unknown backend values are ignored so the sawToolUse
// fallback still applies.
func (a *blockAssembler) setStopReason(kiroStop string) {
	if r := mapStopReason(kiroStop); r != "" {
		a.finalStopReason = r
	}
}

// mapStopReason translates a CodeWhisperer stopReason enum value to the
// corresponding Anthropic stop_reason. Returns "" for unknown / empty input.
func mapStopReason(kiroStop string) string {
	switch strings.ToUpper(strings.TrimSpace(kiroStop)) {
	case "END_TURN":
		return "end_turn"
	case "TOOL_USE":
		return "tool_use"
	case "MAX_TOKENS":
		return "max_tokens"
	case "STOP_SEQUENCE":
		return "stop_sequence"
	}
	return ""
}

// outputChars is a rough proxy for output length (used for usage heuristics).
func (a *blockAssembler) outputChars() int {
	n := 0
	for _, b := range a.blocks {
		n += len(b.Text) + len(b.Thinking) + len(b.Data) + len(b.Input)
	}
	return n
}

// maxKiroToolName is the longest tool name the Kiro runtime accepts; longer
// names are rejected as "Invalid tool use format".
const maxKiroToolName = 64

// shortenToolName returns a Kiro-safe tool name (<= maxKiroToolName bytes). Names
// at or under the limit pass through unchanged. A longer name is shortened to a
// stable prefix plus a hash of the full name: the same input always shortens the
// same way (so the proxy can map a returned name back to the original) and
// distinct long names stay distinct.
func shortenToolName(name string) string {
	if len(name) <= maxKiroToolName {
		return name
	}
	h := toolNameHash(name)
	prefix := maxKiroToolName - 1 - len(h) // 1 byte for the '_' separator
	if prefix < 0 {
		prefix = 0
	}
	return name[:prefix] + "_" + h
}

func toolNameHash(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 16)
}

// toolNameMapFor builds shortened-name -> original-name entries for every tool
// whose name shortenToolName changes. The response assembler uses it to restore
// the original name on tool_use blocks so the client still recognizes the tool.
func toolNameMapFor(tools []anthropicTool) map[string]string {
	var m map[string]string
	for _, t := range tools {
		s := shortenToolName(t.Name)
		if s != t.Name {
			if m == nil {
				m = map[string]string{}
			}
			m[s] = t.Name
		}
	}
	return m
}

// objectToolInput coerces a request-side tool_use "input" to a JSON object,
// which the Kiro runtime (CodeWhisperer) requires. Missing, null, or any
// non-object value (array, string, number, or malformed JSON) is replaced with
// {}, so the upstream never answers with "Invalid tool use format". A
// well-formed object passes through untouched.
func objectToolInput(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return json.RawMessage(`{}`)
	}
	// The only JSON value that begins with '{' is an object; anything else
	// (null, array, scalar, or malformed) is not a valid object input.
	if strings.HasPrefix(s, "{") && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return json.RawMessage(`{}`)
}

// normalizeToolInput ensures tool input is valid JSON, defaulting to {}.
func normalizeToolInput(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	// Wrap invalid fragments so the client still receives valid JSON.
	b, _ := json.Marshal(map[string]string{"_raw": s})
	return b
}

// estimateTokens is a coarse character-based token estimate (~4 chars/token).
func estimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

// ---------------------------------------------------------------------------
// Tool-call marker leak fix.
//
// The Kiro backend occasionally emits a model's tool-call opening marker as
// plain text inside assistantResponseEvent while the actual call still arrives
// as a structured toolUseEvent. Observed on deepseek-3.2, whose marker is
// "<｜DSML｜function_calls" (｜ = U+FF5C); Anthropic-style "<function_calls>" is
// handled too. The marker can be split across streaming frames.
//
// The fix is deliberately minimal: strip a stray opening marker where it dangles
// at the end of a text run. Structured tool calls are untouched, so tools keep
// working. We do NOT parse tool calls out of leaked XML or synthesize tool_use
// blocks: that would risk mangling text and inventing phantom tool calls when a
// model legitimately writes this markup in prose.
//
// Implementation: ingestText buffers a possibly-incomplete trailing marker in
// leakCarry across frames; everything ahead of it flows straight to addText.
// ---------------------------------------------------------------------------

// dsmlFunctionCalls is deepseek's tool-call opening marker (｜ is U+FF5C, a
// fullwidth vertical bar, not an ASCII pipe).
const dsmlFunctionCalls = "<｜DSML｜function_calls"

// toolOpenMarkers are the complete opening markers stripped when they dangle at
// the end of a text run.
var toolOpenMarkers = []string{"<function_calls>", dsmlFunctionCalls}

// toolOpenerRe strips a bare trailing opening marker from a text run.
var toolOpenerRe = regexp.MustCompile(`(?:<function_calls>|<｜DSML｜function_calls>?)\s*$`)

// ingestText is the text entry point used by the event loop. It strips stray
// tool-call opening markers, buffering an ambiguous trailing fragment in
// leakCarry until the next frame (or end of stream) can decide it.
func (a *blockAssembler) ingestText(text string) error {
	if text == "" {
		return nil
	}
	a.leakCarry += text
	hold := pendingMarkerTail(a.leakCarry)
	cut := len(a.leakCarry) - hold
	if err := a.addText(a.leakCarry[:cut]); err != nil {
		return err
	}
	a.leakCarry = a.leakCarry[cut:]
	return nil
}

// flushLeakCarry emits any buffered trailing text, stripping a bare opening
// marker if that is all that was held. Called before the assembler switches to a
// non-text block and once at end of stream (via finish).
func (a *blockAssembler) flushLeakCarry() error {
	if a.leakCarry == "" {
		return nil
	}
	out := stripOpenerSuffix(a.leakCarry)
	a.leakCarry = ""
	return a.addText(out)
}

// finish flushes any held text and closes the open block. Both the streaming and
// non-streaming loops call it once at end of stream in place of closeOpen.
func (a *blockAssembler) finish() error {
	if err := a.flushLeakCarry(); err != nil {
		return err
	}
	return a.closeOpen()
}

// stripOpenerSuffix removes a bare trailing tool-call opening marker, leaving all
// other text untouched.
func stripOpenerSuffix(s string) string {
	return toolOpenerRe.ReplaceAllString(s, "")
}

// pendingMarkerTail returns how many trailing bytes to hold back because they
// might be a tool-call opening marker (possibly split across frames): either a
// partial prefix of a marker, or a complete marker that may be a stray leak.
func pendingMarkerTail(s string) int {
	hold := 0
	for _, tag := range toolOpenMarkers {
		// Longest suffix of s that is a prefix of tag.
		start := len(tag) - 1
		if start > len(s) {
			start = len(s)
		}
		for k := start; k >= 1; k-- {
			if s[len(s)-k:] == tag[:k] {
				if k > hold {
					hold = k
				}
				break
			}
		}
		// A complete marker at the tail is a stray leak; hold all of it so flush
		// can strip it.
		if strings.HasSuffix(s, tag) && len(tag) > hold {
			hold = len(tag)
		}
	}
	if hold > len(s) {
		hold = len(s)
	}
	return hold
}

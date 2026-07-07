package main

import (
	"encoding/json"
	"fmt"
	"strings"
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
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`

	// Reasoning-effort control (native Anthropic field + OpenAI-style alias).
	OutputConfig    *anthropicOutputConfig `json:"output_config,omitempty"`
	ReasoningEffort string                 `json:"reasoning_effort,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// requestedEffort returns the effort level the client asked for, if any.
// The native Anthropic field output_config.effort wins over the reasoning_effort alias.
func requestedEffort(areq *anthropicRequest) string {
	if areq.OutputConfig != nil && areq.OutputConfig.Effort != "" {
		return areq.OutputConfig.Effort
	}
	return areq.ReasoningEffort
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

	// text / thinking
	Text string `json:"text,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []block
	IsError   bool            `json:"is_error,omitempty"`
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
	Type  string          `json:"type"` // "text" | "tool_use"
	Text  string          `json:"text,omitempty"`
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

// ---------------------------------------------------------------------------
// Request translation: Anthropic -> Kiro.
// ---------------------------------------------------------------------------

// buildKiroRequest converts an Anthropic Messages request into the Kiro
// GenerateAssistantResponse payload.
func buildKiroRequest(cfg *Config, areq *anthropicRequest) (*kiroRequest, error) {
	if len(areq.Messages) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}

	modelID := mapModel(areq.Model)
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
					Name:        t.Name,
					Description: t.Description,
					InputSchema: kiroInputSchema{JSON: schema},
				}},
			)
		}
	}

	history = sanitizeHistory(history)

	return &kiroRequest{
		ConversationState: kiroConversationState{
			ChatTriggerType: "MANUAL",
			ConversationID:  newUUID(),
			CurrentMessage:  current,
			History:         history,
		},
		ProfileArn:   "", // filled in by the server after resolution
		AgentMode:    cfg.AgentMode,
		SystemPrompt: system,
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
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool_use":
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				am.ToolUses = append(am.ToolUses, kiroToolUse{
					ToolUseID: b.ID,
					Name:      b.Name,
					Input:     input,
				})
			}
		}
		am.Content = text.String()
		// CodeWhisperer expects some content; keep a placeholder if empty but
		// tool uses exist so the turn is well-formed.
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
	for _, b := range blocks {
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
		}
	}
	um.Content = text.String()
	if len(images) > 0 {
		um.Images = images
	}
	if len(toolResults) > 0 {
		um.UserInputMessageContext = &kiroUserInputMessageContext{ToolResults: toolResults}
		// A tool-result turn often has no text; CodeWhisperer tolerates empty
		// content here as long as toolResults are present.
	}
	if um.Content == "" && len(toolResults) == 0 && len(images) == 0 {
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

	index    int
	openKind string // "", "text", "tool_use"

	textBuf      strings.Builder
	toolID       string
	toolName     string
	toolInputBuf strings.Builder

	blocks     []anthropicRespBlock
	sawToolUse bool
}

func newBlockAssembler(emit emitFunc) *blockAssembler {
	return &blockAssembler{emit: emit}
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
	newTool := ev.ToolUseID != "" && (a.openKind != "tool_use" || ev.ToolUseID != a.toolID)
	if newTool {
		if err := a.closeOpen(); err != nil {
			return err
		}
		a.openKind = "tool_use"
		a.toolID = ev.ToolUseID
		a.toolName = ev.ToolName
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
func (a *blockAssembler) stopReason() string {
	if a.sawToolUse {
		return "tool_use"
	}
	return "end_turn"
}

// outputChars is a rough proxy for output length (used for usage heuristics).
func (a *blockAssembler) outputChars() int {
	n := 0
	for _, b := range a.blocks {
		n += len(b.Text) + len(b.Input)
	}
	return n
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

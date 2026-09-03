package main

// ---------------------------------------------------------------------------
// OpenAI Responses API subset (POST /v1/responses).
//
// Served for Responses-only clients (Codex CLI since it dropped chat wire),
// translated to the same internal anthropicRequest + kiro stream path as
// /v1/messages. Stateless by design: the stateless subset (store:false, full
// history in input) is the only mode Codex and friends use, so there is no
// server-side conversation store and previous_response_id is rejected.
//
// Server-executed tools (web_search, file_search, code_interpreter, computer
// use, hosted MCP) have no Kiro counterpart and are rejected with a plain 400
// before the stream starts — the Responses protocol has no capability
// advertisement, so a runtime rejection IS the capability signal.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Request wire types. Only the fields we translate or explicitly gate are
// modelled; unknown fields are ignored like everywhere else in this proxy.
// ---------------------------------------------------------------------------

type responsesRequest struct {
	Model              string              `json:"model"`
	Instructions       string              `json:"instructions"`
	Input              json.RawMessage     `json:"input"`
	Stream             bool                `json:"stream"`
	MaxOutputTokens    int                 `json:"max_output_tokens"`
	Temperature        *float64            `json:"temperature"`
	TopP               *float64            `json:"top_p"`
	Tools              []responsesTool     `json:"tools"`
	Reasoning          *responsesReasoning `json:"reasoning"`
	PreviousResponseID string              `json:"previous_response_id"`
	Background         bool                `json:"background"`
	// Accepted and ignored: the stateless Codex shape carries them, but none
	// of them changes the translation.
	Store             *bool           `json:"store"`
	Include           []string        `json:"include"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	PromptCacheKey    string          `json:"prompt_cache_key"`
	ServiceTier       string          `json:"service_tier"`
	Metadata          json.RawMessage `json:"metadata"`
	Text              json.RawMessage `json:"text"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	SafetyIdentifier  string          `json:"safety_identifier"`
}

type responsesReasoning struct {
	Effort  string          `json:"effort"`  // minimal | low | medium | high (xhigh/max on Anthropic-style backends)
	Summary json.RawMessage `json:"summary"` // accepted; no summary events are synthesized in v1
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

// responsesItem is one input item. Content/Output stay raw because both are
// "string or part array" unions parsed per item kind.
type responsesItem struct {
	Type string `json:"type"` // message | function_call | function_call_output | reasoning
	Role string `json:"role"` // message: user | assistant | system | developer

	Content json.RawMessage `json:"content"` // message

	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	// function_call_output
	Output json.RawMessage `json:"output"`

	// reasoning (accepted from Codex replays and dropped)
	ID      string          `json:"id"`
	Summary json.RawMessage `json:"summary"`
}

// responsesError writes an OpenAI-shaped error ({"error":{type,message}}).
func writeResponsesError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	noteError(r.Context(), message)
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": code, "message": message},
	})
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponsesError(w, r, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed; POST the Responses payload to this endpoint")
		return
	}
	if !s.authorized(r) {
		writeResponsesError(w, r, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024*1024))
	if err != nil {
		writeResponsesError(w, r, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponsesError(w, r, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if err := validateResponsesRequest(&req); err != nil {
		writeResponsesError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	areq, err := responsesToAnthropic(&req)
	if err != nil {
		writeResponsesError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if info := accessFrom(r.Context()); info != nil {
		info.model = areq.Model
		info.stream = areq.Stream
	}

	// Same image handling as /v1/messages: inline remote URLs in the current
	// turn, drop history images to placeholders.
	processImages(r.Context(), &areq, newImageFetcher(s.kiro.client))

	kreq, err := buildKiroRequest(s.cfg, &areq)
	if err != nil {
		writeResponsesError(w, r, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Pre-stream account selection with failover, identical to /v1/messages.
	// Everything below this line that fails mid-stream must terminate the SSE
	// with response.failed instead of a bare disconnect.
	stream, err := s.openStream(r.Context(), kreq, &areq)
	if err != nil {
		status, code := mapUpstreamResponsesError(err)
		writeResponsesError(w, r, status, code, err.Error())
		return
	}
	defer stream.Close()

	inputChars := len(extractText(areq.System))
	for _, m := range areq.Messages {
		inputChars += len(m.Content)
	}

	if req.Stream {
		s.streamResponses(w, r, &req, &areq, stream, inputChars)
		return
	}
	s.aggregateResponses(w, r, &req, &areq, stream, inputChars)
}

// validateResponsesRequest enforces the supported subset up front so clients
// get plain HTTP errors rather than mid-stream failures.
func validateResponsesRequest(req *responsesRequest) error {
	if req.PreviousResponseID != "" {
		return errors.New("previous_response_id is not supported by this provider; send the full conversation in input (stateless mode)")
	}
	if req.Background {
		return errors.New("background mode is not supported by this provider")
	}
	var unsupported []string
	for _, t := range req.Tools {
		if t.Type != "function" {
			unsupported = append(unsupported, t.Type)
		}
	}
	if len(unsupported) > 0 {
		return fmt.Errorf(
			"tool type(s) not supported by this provider: %s. This endpoint executes only client-side function tools (server-executed tools like web_search run on OpenAI infrastructure and have no counterpart here); remove them from tools",
			strings.Join(unsupported, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Request conversion: responses items -> anthropicRequest
// ---------------------------------------------------------------------------

// responsesToAnthropic maps the Responses request onto the internal Anthropic
// request consumed by buildKiroRequest. Consecutive function_call items merge
// into one assistant turn and consecutive function_call_output items into one
// user turn, mirroring the Anthropic tool_use/tool_result block shape.
func responsesToAnthropic(req *responsesRequest) (anthropicRequest, error) {
	areq := anthropicRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxOutputTokens,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	if req.Instructions != "" {
		sys, err := json.Marshal(req.Instructions)
		if err != nil {
			return areq, err
		}
		areq.System = sys
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		// Responses effort vocabulary is a subset; minimal clamps to low.
		effort := req.Reasoning.Effort
		if effort == "minimal" {
			effort = "low"
		}
		areq.OutputConfig = &anthropicOutputConfig{Effort: effort}
	}
	for _, t := range req.Tools {
		areq.Tools = append(areq.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}

	items, err := parseResponsesItems(req.Input)
	if err != nil {
		return areq, err
	}

	var systemText strings.Builder
	appendSystem := func(text string) {
		if systemText.Len() > 0 {
			systemText.WriteString("\n\n")
		}
		systemText.WriteString(text)
	}

	flushCalls := func(calls []anthropicContentBlock) {
		if len(calls) == 0 {
			return
		}
		areq.Messages = append(areq.Messages, anthropicMessage{Role: "assistant", Content: mustJSON(calls)})
	}
	flushOutputs := func(outputs []anthropicContentBlock) {
		if len(outputs) == 0 {
			return
		}
		areq.Messages = append(areq.Messages, anthropicMessage{Role: "user", Content: mustJSON(outputs)})
	}

	var pendingCalls, pendingOutputs []anthropicContentBlock
	for i := range items {
		item := &items[i]
		switch item.Type {
		case "message", "":
			// "" is the OpenAI EasyInputMessage form: type is optional and
			// defaults to "message" (lean clients like omp send role+content
			// only).
			flushCalls(pendingCalls)
			pendingCalls = nil
			flushOutputs(pendingOutputs)
			pendingOutputs = nil
			role := item.Role
			if role == "" {
				role = "user"
			}
			text, blocks, err := responsesContentToBlocks(item.Content)
			if err != nil {
				return areq, err
			}
			switch role {
			case "assistant":
				areq.Messages = append(areq.Messages, anthropicMessage{Role: "assistant", Content: contentOrString(text, blocks)})
			case "user":
				areq.Messages = append(areq.Messages, anthropicMessage{Role: "user", Content: contentOrString(text, blocks)})
			case "system", "developer":
				// Responses system/developer items fold into the instructions.
				appendSystem(text)
			default:
				return areq, fmt.Errorf("input item %d: unsupported message role %q", i, role)
			}
		case "function_call":
			flushOutputs(pendingOutputs)
			pendingOutputs = nil
			if item.CallID == "" || item.Name == "" {
				return areq, fmt.Errorf("input item %d: function_call requires call_id and name", i)
			}
			pendingCalls = append(pendingCalls, anthropicContentBlock{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: normalizeToolInput(item.Arguments),
			})
		case "function_call_output":
			flushCalls(pendingCalls)
			pendingCalls = nil
			if item.CallID == "" {
				return areq, fmt.Errorf("input item %d: function_call_output requires call_id", i)
			}
			block, err := responsesToolResultBlock(item)
			if err != nil {
				return areq, err
			}
			pendingOutputs = append(pendingOutputs, block)
		case "reasoning":
			// Encrypted reasoning replay is an OpenAI-side blob with no Kiro
			// counterpart; the thinking round-trip is handled by the backend
			// conversation instead. Drop silently.
		default:
			return areq, fmt.Errorf("input item %d: unsupported type %q", i, item.Type)
		}
	}
	flushCalls(pendingCalls)
	flushOutputs(pendingOutputs)

	if systemText.Len() > 0 {
		extra := systemText.String()
		if len(areq.System) == 0 {
			areq.System = mustJSON(extra)
		} else {
			var base string
			_ = json.Unmarshal(areq.System, &base)
			areq.System = mustJSON(strings.TrimSpace(base + "\n\n" + extra))
		}
	}

	return areq, nil
}

// parseResponsesItems accepts the two input shapes: a bare string (single
// user message) or an item array.
func parseResponsesItems(raw json.RawMessage) ([]responsesItem, error) {
	if len(raw) == 0 {
		return nil, errors.New("input is required")
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return []responsesItem{{Type: "message", Role: "user", Content: raw}}, nil
	}
	var items []responsesItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("input: must be a string or an array of items: %w", err)
	}
	return items, nil
}

// responsesContentToBlocks flattens message content (string or part array)
// into plain text plus Anthropic content blocks for non-text parts.
func responsesContentToBlocks(raw json.RawMessage) (text string, blocks []anthropicContentBlock, err error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, fmt.Errorf("message content: must be a string or an array of content parts: %w", err)
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text":
			sb.WriteString(p.Text)
		case "input_image", "output_image":
			if p.ImageURL == "" {
				return "", nil, errors.New("input_image part requires image_url")
			}
			// URL sources are inlined by processImages exactly as on the
			// Anthropic path; data URLs decode there too.
			blocks = append(blocks, anthropicContentBlock{
				Type: "image",
				Source: &anthropicImageSource{
					Type: "url",
					URL:  p.ImageURL,
				},
			})
		default:
			return "", nil, fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	return sb.String(), blocks, nil
}

// contentOrString keeps plain-text messages as JSON strings (matching what
// Claude Code sends) and falls back to the block array when images exist.
func contentOrString(text string, blocks []anthropicContentBlock) json.RawMessage {
	if len(blocks) == 0 {
		return mustJSON(text)
	}
	if text != "" {
		blocks = append([]anthropicContentBlock{{Type: "text", Text: text}}, blocks...)
	}
	return mustJSON(blocks)
}

// responsesToolResultBlock maps a function_call_output to an Anthropic
// tool_result block. Output is either a string or an array of parts
// (input_text / input_image); unrecognized parts survive as raw JSON.
func responsesToolResultBlock(item *responsesItem) (anthropicContentBlock, error) {
	block := anthropicContentBlock{
		Type:      "tool_result",
		ToolUseID: item.CallID,
	}
	if len(item.Output) == 0 {
		block.Content = mustJSON("")
		return block, nil
	}
	var asString string
	if err := json.Unmarshal(item.Output, &asString); err == nil {
		block.Content = mustJSON(asString)
		return block, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(item.Output, &parts); err != nil {
		// Opaque payload: keep the raw JSON so nothing is lost.
		block.Content = item.Output
		return block, nil
	}
	out := make([]anthropicContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text":
			out = append(out, anthropicContentBlock{Type: "text", Text: p.Text})
		default:
			raw, _ := json.Marshal(p)
			out = append(out, anthropicContentBlock{Type: "raw_part", Text: string(raw)})
		}
	}
	if len(out) == 0 {
		out = append(out, anthropicContentBlock{Type: "text", Text: ""})
	}
	block.Content = mustJSON(out)
	return block, nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// ---------------------------------------------------------------------------
// Stream synthesis: Kiro events -> Responses SSE (or an aggregated response).
// A native item-model assembler, not a blockAdapter: Responses output items
// (message / function_call) are tracked directly with their own output_index.
// ---------------------------------------------------------------------------

type responsesEmitFunc func(event string, payload map[string]any) error

type responsesAssembler struct {
	emit    responsesEmitFunc // nil in non-stream mode
	nameMap map[string]string // shortened -> original tool names
	respID  string
	model   string
	created int64

	seq     int64  // response-wide sequence_number, strictly increasing
	outIdx  int    // next output_index
	open    string // "", "message", "function_call"
	msgID   string
	msgText strings.Builder
	fcID    string // item id (fc_...)
	fcCall  string // call_id echoed back to the client
	fcName  string // original (unshortened) tool name
	fcArgs  strings.Builder
	items   []map[string]any // completed-snapshot output items in order

	leakCarry  string
	outChars   int
	stopReason string
	// lastError records the mid-stream failure for the non-stream path, which
	// surfaces it as a plain HTTP error instead of an embedded failed object.
	lastError *responsesStreamError
}

type responsesStreamError struct {
	status  int
	code    string
	message string
}

func newResponsesAssembler(emit responsesEmitFunc, nameMap map[string]string, respID, model string) *responsesAssembler {
	return &responsesAssembler{
		emit:    emit,
		nameMap: nameMap,
		respID:  respID,
		model:   model,
		created: time.Now().Unix(),
	}
}

// send stamps the sequence number and writes one SSE frame (no-op when nil).
func (a *responsesAssembler) send(event string, payload map[string]any) error {
	if a.emit == nil {
		return nil
	}
	a.seq++
	payload["sequence_number"] = a.seq
	return a.emit(event, payload)
}

// responseEnvelope builds the shared response object fields.
func (a *responsesAssembler) responseEnvelope(status string) map[string]any {
	return map[string]any{
		"id":                 a.respID,
		"object":             "response",
		"created_at":         a.created,
		"status":             status,
		"model":              a.model,
		"error":              nil,
		"incomplete_details": nil,
		"output":             []any{},
		"usage":              nil,
	}
}

// start emits response.created + response.in_progress. Call once after the
// upstream stream is open; from here on every exit path must terminate with a
// terminal event (completed / incomplete / failed).
func (a *responsesAssembler) start() error {
	created := a.responseEnvelope("in_progress")
	if err := a.send("response.created", map[string]any{"type": "response.created", "response": created}); err != nil {
		return err
	}
	return a.send("response.in_progress", map[string]any{"type": "response.in_progress", "response": a.responseEnvelope("in_progress")})
}

// originalToolName restores a request-side shortened tool name.
func (a *responsesAssembler) originalToolName(name string) string {
	if a.nameMap == nil {
		return name
	}
	if orig, ok := a.nameMap[name]; ok {
		return orig
	}
	return name
}

// feedText ingests one assistantResponseEvent chunk through the leak filter
// (identical carry semantics to the Anthropic assembler) and emits deltas.
func (a *responsesAssembler) feedText(text string) error {
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

// flushLeakCarry emits any held trailing text, stripping a bare opening
// marker if that is all that was held. Called before switching away from text
// and once at end of stream.
func (a *responsesAssembler) flushLeakCarry() error {
	if a.leakCarry == "" {
		return nil
	}
	out := stripOpenerSuffix(a.leakCarry)
	a.leakCarry = ""
	return a.addText(out)
}

// addText opens/continues the message item and emits an output_text.delta.
func (a *responsesAssembler) addText(text string) error {
	if text == "" {
		return nil
	}
	if a.open != "message" {
		if err := a.closeOpen(); err != nil {
			return err
		}
		a.open = "message"
		a.msgID = "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		a.msgText.Reset()
		idx := a.outIdx
		a.outIdx++
		if err := a.send("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": idx,
			"item": map[string]any{
				"type":    "message",
				"role":    "assistant",
				"id":      a.msgID,
				"status":  "in_progress",
				"content": []any{},
			},
		}); err != nil {
			return err
		}
		if err := a.send("response.content_part.added", map[string]any{
			"type":          "response.content_part.added",
			"item_id":       a.msgID,
			"output_index":  idx,
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        "",
				"annotations": []any{},
			},
		}); err != nil {
			return err
		}
	}
	a.msgText.WriteString(text)
	a.outChars += len(text)
	return a.send("response.output_text.delta", map[string]any{
		"type":          "response.output_text.delta",
		"item_id":       a.msgID,
		"output_index":  a.outIdx - 1,
		"content_index": 0,
		"delta":         text,
		"logprobs":      []any{},
	})
}

// addToolUse opens/continues a function_call item from a Kiro toolUseEvent.
func (a *responsesAssembler) addToolUse(ev *kiroEvent) error {
	if err := a.flushLeakCarry(); err != nil {
		return err
	}
	newTool := ev.ToolUseID != "" && (a.open != "function_call" || ev.ToolUseID != a.fcCall)
	if newTool {
		if err := a.closeOpen(); err != nil {
			return err
		}
		a.open = "function_call"
		a.fcID = "fc_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		a.fcCall = ev.ToolUseID
		a.fcName = a.originalToolName(ev.ToolName)
		a.fcArgs.Reset()
		idx := a.outIdx
		a.outIdx++
		if err := a.send("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": idx,
			"item": map[string]any{
				"type":      "function_call",
				"id":        a.fcID,
				"call_id":   a.fcCall,
				"name":      a.fcName,
				"arguments": "",
				"status":    "in_progress",
			},
		}); err != nil {
			return err
		}
	} else if a.open != "function_call" {
		// A tool chunk with no id and nothing open: ignore (mirrors the
		// Anthropic assembler).
		return nil
	}
	if ev.ToolInput != "" {
		a.fcArgs.WriteString(ev.ToolInput)
		a.outChars += len(ev.ToolInput)
		if err := a.send("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      a.fcID,
			"output_index": a.outIdx - 1,
			"delta":        ev.ToolInput,
		}); err != nil {
			return err
		}
	}
	if ev.ToolStop {
		return a.closeOpen()
	}
	return nil
}

// closeOpen terminates the open item: done-deltas carrying the full text /
// arguments, then output_item.done. The finished item is appended to the
// completed snapshot.
func (a *responsesAssembler) closeOpen() error {
	switch a.open {
	case "message":
		a.open = ""
		idx := a.outIdx - 1
		full := a.msgText.String()
		if err := a.send("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       a.msgID,
			"output_index":  idx,
			"content_index": 0,
			"text":          full,
		}); err != nil {
			return err
		}
		if err := a.send("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       a.msgID,
			"output_index":  idx,
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        full,
				"annotations": []any{},
			},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type":   "message",
			"role":   "assistant",
			"id":     a.msgID,
			"status": "completed",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        full,
				"annotations": []any{},
			}},
		}
		if err := a.send("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": idx,
			"item":         item,
		}); err != nil {
			return err
		}
		a.items = append(a.items, item)
		a.msgText.Reset()
	case "function_call":
		a.open = ""
		idx := a.outIdx - 1
		// Always emit valid JSON: Codex parses arguments, and an empty
		// string would fail; normalizeToolInput defaults to {} (same as the
		// Anthropic path).
		full := string(normalizeToolInput(a.fcArgs.String()))
		if err := a.send("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      a.fcID,
			"output_index": idx,
			"arguments":    full,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type":      "function_call",
			"id":        a.fcID,
			"call_id":   a.fcCall,
			"name":      a.fcName,
			"arguments": full,
			"status":    "completed",
		}
		if err := a.send("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": idx,
			"item":         item,
		}); err != nil {
			return err
		}
		a.items = append(a.items, item)
		a.fcArgs.Reset()
	}
	return nil
}

// finish flushes held text and closes the open item. End-of-stream only.
func (a *responsesAssembler) finish() error {
	if err := a.flushLeakCarry(); err != nil {
		return err
	}
	return a.closeOpen()
}

// usage builds the Responses usage object (same coarse estimator as the
// Anthropic path; Kiro exposes no authoritative token counts).
func (a *responsesAssembler) usage(inputChars int) map[string]any {
	in := estimateTokens(inputChars)
	out := estimateTokens(a.outChars)
	return map[string]any{
		"input_tokens":  in,
		"output_tokens": out,
		"total_tokens":  in + out,
		"input_tokens_details": map[string]any{
			"cached_tokens": 0,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
	}
}

// complete emits the terminal completed (or incomplete) event carrying the
// full output snapshot. Some clients only read completed.output, so the array
// is always the authoritative item list.
func (a *responsesAssembler) complete(inputChars int) error {
	if a.items == nil {
		a.items = []map[string]any{}
	}
	resp := a.responseEnvelope("completed")
	resp["output"] = a.items
	resp["usage"] = a.usage(inputChars)
	event := "response.completed"
	if mapStopReason(a.stopReason) == "max_tokens" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		event = "response.incomplete"
	}
	return a.send(event, map[string]any{"type": event, "response": resp})
}

// failed emits the terminal response.failed event. error codes reuse the
// upstream classification; rate_limit_exceeded and context_length_exceeded
// are the codes Codex gives retry/fatal semantics to.
func (a *responsesAssembler) failed(errType, message string) error {
	resp := a.responseEnvelope("failed")
	resp["error"] = map[string]any{
		"code":    responsesErrorCode(errType, message),
		"message": message,
	}
	return a.send("response.failed", map[string]any{"type": "response.failed", "response": resp})
}

// responsesErrorCode maps the Anthropic-ish error classification carried by
// mapUpstreamEventError onto Responses error codes.
func responsesErrorCode(errType, message string) string {
	switch errType {
	case "rate_limit_error":
		return "rate_limit_exceeded"
	case "invalid_request_error":
		return "invalid_request_error"
	case "authentication_error":
		return "authentication_error"
	case "permission_error":
		return "permission_error"
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "context"), strings.Contains(lower, "too long"), strings.Contains(lower, "prompt is too long"):
		return "context_length_exceeded"
	case strings.Contains(lower, "quota"), strings.Contains(lower, "credit"), strings.Contains(lower, "billing"):
		return "insufficient_quota"
	}
	return "api_error"
}

// mapUpstreamResponsesError maps an openStream failure to HTTP status plus
// Responses error code (pre-stream, so plain HTTP, not SSE).
func mapUpstreamResponsesError(err error) (int, string) {
	if errors.Is(err, errNoAccount) {
		return http.StatusServiceUnavailable, "api_error"
	}
	if errors.Is(err, errModelUnavailable) {
		return http.StatusBadRequest, "invalid_request_error"
	}
	status, errType := mapUpstreamError(err)
	return status, responsesErrorCode(errType, err.Error())
}

// consumeResponsesStream drives the shared event loop for both stream and
// non-stream modes. It never writes HTTP errors; mid-stream failures surface
// as response.failed via the assembler.
func consumeResponsesStream(r *http.Request, asm *responsesAssembler, stream *kiroStream, inputChars int) {
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Client disconnect: nothing left to emit onto.
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				noteCanceled(r.Context())
				return
			}
			noteError(r.Context(), "stream read error: "+err.Error())
			asm.lastError = &responsesStreamError{status: http.StatusBadGateway, code: "api_error", message: "stream read error: " + err.Error()}
			_ = asm.failed("api_error", "stream read error: "+err.Error())
			return
		}
		switch ev.Kind {
		case evReasoning:
			// v1 drops reasoning content entirely; flush any dangling leak
			// carry first so text order stays intact.
			if err := asm.flushLeakCarry(); err != nil {
				return
			}
		case evText:
			if err := asm.feedText(ev.Text); err != nil {
				return
			}
		case evToolUse:
			if err := asm.addToolUse(ev); err != nil {
				return
			}
		case evMetadata:
			asm.stopReason = ev.StopReason
		case evError:
			status, errType, message := mapUpstreamEventError(ev)
			noteError(r.Context(), message)
			asm.lastError = &responsesStreamError{status: status, code: responsesErrorCode(errType, message), message: message}
			_ = asm.failed(errType, message)
			return
		}
	}
	if err := asm.finish(); err != nil {
		return
	}
	_ = asm.complete(inputChars)
}

// streamResponses writes the Responses SSE stream.
func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, req *responsesRequest, areq *anthropicRequest, stream *kiroStream, inputChars int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeResponsesError(w, r, http.StatusInternalServerError, "api_error", "streaming unsupported by server")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(event string, payload map[string]any) error {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	asm := newResponsesAssembler(emit, toolNameMapFor(areq.Tools),
		"resp_"+strings.ReplaceAll(uuid.NewString(), "-", ""), responseModel(areq, stream))
	if asm.start() != nil {
		return
	}
	consumeResponsesStream(r, asm, stream, inputChars)
}

// aggregateResponses returns one Responses JSON object (stream=false).
func (s *Server) aggregateResponses(w http.ResponseWriter, r *http.Request, req *responsesRequest, areq *anthropicRequest, stream *kiroStream, inputChars int) {
	asm := newResponsesAssembler(nil, toolNameMapFor(areq.Tools),
		"resp_"+strings.ReplaceAll(uuid.NewString(), "-", ""), responseModel(areq, stream))
	consumeResponsesStream(r, asm, stream, inputChars)

	// A mid-stream failure in non-stream mode surfaces as an HTTP error, not
	// an embedded failed response.
	if last := asm.lastError; last != nil {
		writeResponsesError(w, r, last.status, last.code, last.message)
		return
	}

	if asm.items == nil {
		asm.items = []map[string]any{}
	}
	resp := asm.responseEnvelope("completed")
	resp["output"] = asm.items
	resp["usage"] = asm.usage(inputChars)
	if mapStopReason(asm.stopReason) == "max_tokens" {
		resp["status"] = "incomplete"
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	writeJSON(w, http.StatusOK, resp)
}

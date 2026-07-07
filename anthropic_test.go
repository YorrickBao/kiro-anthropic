package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMapModel(t *testing.T) {
	cases := map[string]string{
		"":                           "auto",
		"auto":                       "auto",
		"claude-opus-4.8":            "claude-opus-4.8", // known passthrough
		"claude-opus-4-8-20260101":   "claude-opus-4.8", // keyword: opus
		"claude-3-5-sonnet-20241022": "claude-sonnet-4.5",
		"claude-3-5-haiku-latest":    "claude-haiku-4.5",
		"gpt-4o":                     "auto",           // unknown, no keyword, no dot
		"some-model-1.2":             "some-model-1.2", // dotted -> passthrough
		"glm-5":                      "glm-5",
	}
	for in, want := range cases {
		if got := mapModel(in); got != want {
			t.Errorf("mapModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMediaTypeToKiroImageFormat(t *testing.T) {
	ok := map[string]string{
		"image/png": "png", "image/jpeg": "jpeg", "image/jpg": "jpeg",
		"image/gif": "gif", "image/webp": "webp", "IMAGE/PNG": "png", " image/png ": "png",
	}
	for mt, want := range ok {
		if got, valid := mediaTypeToKiroImageFormat(mt); !valid || got != want {
			t.Errorf("mediaTypeToKiroImageFormat(%q) = %q,%v; want %q,true", mt, got, valid, want)
		}
	}
	for _, mt := range []string{"", "image/svg+xml", "text/plain"} {
		if _, valid := mediaTypeToKiroImageFormat(mt); valid {
			t.Errorf("mediaTypeToKiroImageFormat(%q) should be invalid", mt)
		}
	}
}

func TestConvertImage(t *testing.T) {
	b := anthropicContentBlock{Type: "image", Source: &anthropicImageSource{
		Type: "base64", MediaType: "image/png", Data: "QUJD"}}
	img, ok := convertImage(b)
	if !ok || img.Format != "png" || img.Source.Bytes != "QUJD" {
		t.Errorf("convertImage base64 = %+v,%v", img, ok)
	}
	// url source (no data) -> skipped
	if _, ok := convertImage(anthropicContentBlock{Type: "image",
		Source: &anthropicImageSource{Type: "url", URL: "http://x/y.png"}}); ok {
		t.Errorf("url image should be skipped")
	}
	// unsupported media type -> skipped
	if _, ok := convertImage(anthropicContentBlock{Type: "image",
		Source: &anthropicImageSource{Type: "base64", MediaType: "image/svg+xml", Data: "QQ=="}}); ok {
		t.Errorf("svg image should be skipped")
	}
}

func TestRequestedEffort(t *testing.T) {
	if got := requestedEffort(&anthropicRequest{
		OutputConfig: &anthropicOutputConfig{Effort: "high"}, ReasoningEffort: "low"}); got != "high" {
		t.Errorf("output_config.effort should win, got %q", got)
	}
	if got := requestedEffort(&anthropicRequest{ReasoningEffort: "medium"}); got != "medium" {
		t.Errorf("reasoning_effort fallback, got %q", got)
	}
	if got := requestedEffort(&anthropicRequest{}); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestNormalizeToolInput(t *testing.T) {
	if string(normalizeToolInput("")) != `{}` {
		t.Errorf("empty should be {}")
	}
	if string(normalizeToolInput(`{"a":1}`)) != `{"a":1}` {
		t.Errorf("valid json should pass through")
	}
	got := normalizeToolInput("not json")
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil || m["_raw"] != "not json" {
		t.Errorf("invalid json should be wrapped, got %s", got)
	}
}

func TestExtractText(t *testing.T) {
	if got := extractText(json.RawMessage(`"hi"`)); got != "hi" {
		t.Errorf("string system = %q", got)
	}
	if got := extractText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)); got != "ab" {
		t.Errorf("array system = %q", got)
	}
	if got := extractText(nil); got != "" {
		t.Errorf("nil system = %q", got)
	}
}

func TestConvertToolResult(t *testing.T) {
	// string content
	tr := convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t1",
		Content: json.RawMessage(`"result text"`)})
	if tr.ToolUseID != "t1" || tr.Status != "success" || len(tr.Content) != 1 || tr.Content[0].Text != "result text" {
		t.Errorf("string tool_result = %+v", tr)
	}
	// error flag
	tr = convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t2",
		IsError: true, Content: json.RawMessage(`"boom"`)})
	if tr.Status != "error" {
		t.Errorf("is_error should map to error status, got %q", tr.Status)
	}
	// array of text blocks
	tr = convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t3",
		Content: json.RawMessage(`[{"type":"text","text":"x"}]`)})
	if len(tr.Content) != 1 || tr.Content[0].Text != "x" {
		t.Errorf("array tool_result = %+v", tr)
	}
	// array with a non-text block -> preserved as JSON, not dropped
	tr = convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t4",
		Content: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QQ=="}}]`)})
	if len(tr.Content) != 1 || tr.Content[0].Text != "" || len(tr.Content[0].JSON) == 0 {
		t.Errorf("non-text tool_result should be JSON, got %+v", tr.Content)
	}
	if !strings.Contains(string(tr.Content[0].JSON), "image") {
		t.Errorf("non-text tool_result JSON = %s", tr.Content[0].JSON)
	}
}

func TestBuildKiroRequestBasic(t *testing.T) {
	areq := &anthropicRequest{
		Model:    "claude-opus-4.8",
		System:   json.RawMessage(`"you are helpful"`),
		Messages: []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}
	k, err := buildKiroRequest(&Config{AgentMode: "vibe"}, areq)
	if err != nil {
		t.Fatalf("buildKiroRequest: %v", err)
	}
	if k.ConversationState.ChatTriggerType != "MANUAL" {
		t.Errorf("chatTriggerType = %q", k.ConversationState.ChatTriggerType)
	}
	if k.SystemPrompt != "you are helpful" {
		t.Errorf("systemPrompt = %q", k.SystemPrompt)
	}
	if k.AgentMode != "vibe" {
		t.Errorf("agentMode = %q", k.AgentMode)
	}
	um := k.ConversationState.CurrentMessage.UserInputMessage
	if um == nil || um.Content != "hello" || um.ModelID != "claude-opus-4.8" || um.Origin != "AI_EDITOR" {
		t.Errorf("current message = %+v", um)
	}
	if len(k.ConversationState.History) != 0 {
		t.Errorf("history should be empty, got %d", len(k.ConversationState.History))
	}
}

func TestBuildKiroRequestMultiTurnAndTools(t *testing.T) {
	areq := &anthropicRequest{
		Model: "claude-opus-4.8",
		Tools: []anthropicTool{{Name: "get_weather", Description: "w",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"weather?"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"tu1","name":"get_weather","input":{"city":"NYC"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu1","content":"sunny"}]`)},
		},
	}
	k, err := buildKiroRequest(&Config{}, areq)
	if err != nil {
		t.Fatalf("buildKiroRequest: %v", err)
	}
	// current = last user (tool_result)
	cur := k.ConversationState.CurrentMessage.UserInputMessage
	if cur == nil || cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("current tool_result missing: %+v", cur)
	}
	if cur.UserInputMessageContext.ToolResults[0].ToolUseID != "tu1" {
		t.Errorf("tool_result id = %q", cur.UserInputMessageContext.ToolResults[0].ToolUseID)
	}
	// tools attached to current message
	if len(cur.UserInputMessageContext.Tools) != 1 || cur.UserInputMessageContext.Tools[0].ToolSpecification.Name != "get_weather" {
		t.Errorf("tools not attached: %+v", cur.UserInputMessageContext.Tools)
	}
	// history: [user, assistant] and starts with user
	if len(k.ConversationState.History) != 2 {
		t.Fatalf("history len = %d", len(k.ConversationState.History))
	}
	if k.ConversationState.History[0].UserInputMessage == nil {
		t.Errorf("history should start with a user turn")
	}
	if am := k.ConversationState.History[1].AssistantResponseMessage; am == nil || len(am.ToolUses) != 1 || am.ToolUses[0].ToolUseID != "tu1" {
		t.Errorf("assistant tool_use not converted: %+v", k.ConversationState.History[1])
	}
}

func TestBuildKiroRequestImage(t *testing.T) {
	areq := &anthropicRequest{
		Model: "claude-opus-4.8",
		Messages: []anthropicMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"what is this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]`)}},
	}
	k, err := buildKiroRequest(&Config{}, areq)
	if err != nil {
		t.Fatalf("buildKiroRequest: %v", err)
	}
	um := k.ConversationState.CurrentMessage.UserInputMessage
	if len(um.Images) != 1 || um.Images[0].Format != "png" || um.Images[0].Source.Bytes != "QUJD" {
		t.Errorf("image not converted: %+v", um.Images)
	}
}

func TestBuildKiroRequestEmptyMessages(t *testing.T) {
	if _, err := buildKiroRequest(&Config{}, &anthropicRequest{}); err == nil {
		t.Errorf("expected error for empty messages")
	}
}

// When the last message is an assistant turn (prefill), the current message is
// synthesized as a minimal user turn and everything goes into history.
func TestBuildKiroRequestAssistantLast(t *testing.T) {
	areq := &anthropicRequest{
		Model: "claude-opus-4.8",
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`"partial answer"`)},
		},
	}
	k, err := buildKiroRequest(&Config{}, areq)
	if err != nil {
		t.Fatalf("buildKiroRequest: %v", err)
	}
	cur := k.ConversationState.CurrentMessage.UserInputMessage
	if cur == nil || cur.Content != " " || cur.ModelID != "claude-opus-4.8" {
		t.Errorf("synthesized current = %+v", cur)
	}
	if len(k.ConversationState.History) != 2 {
		t.Fatalf("history len = %d, want 2", len(k.ConversationState.History))
	}
	if k.ConversationState.History[0].UserInputMessage == nil {
		t.Errorf("history[0] should be the user turn")
	}
	if am := k.ConversationState.History[1].AssistantResponseMessage; am == nil || am.Content != "partial answer" {
		t.Errorf("history[1] assistant = %+v", k.ConversationState.History[1])
	}
}

// --- blockAssembler ---

func TestBlockAssemblerText(t *testing.T) {
	a := newBlockAssembler(nil)
	_ = a.addText("Hello")
	_ = a.addText(" world")
	_ = a.closeOpen()
	if len(a.blocks) != 1 || a.blocks[0].Type != "text" || a.blocks[0].Text != "Hello world" {
		t.Errorf("text blocks = %+v", a.blocks)
	}
	if a.stopReason() != "end_turn" {
		t.Errorf("stopReason = %q", a.stopReason())
	}
}

func TestBlockAssemblerToolUse(t *testing.T) {
	a := newBlockAssembler(nil)
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1", ToolName: "get_weather", ToolInput: `{"city":`})
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1", ToolInput: `"NYC"}`, ToolStop: true})
	if len(a.blocks) != 1 {
		t.Fatalf("blocks = %+v", a.blocks)
	}
	b := a.blocks[0]
	if b.Type != "tool_use" || b.ID != "t1" || b.Name != "get_weather" || string(b.Input) != `{"city":"NYC"}` {
		t.Errorf("tool_use block = %+v (input %s)", b, b.Input)
	}
	if a.stopReason() != "tool_use" {
		t.Errorf("stopReason = %q", a.stopReason())
	}
}

func TestBlockAssemblerTextThenTool(t *testing.T) {
	a := newBlockAssembler(nil)
	_ = a.addText("Answer:")
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t9", ToolName: "f", ToolInput: `{}`, ToolStop: true})
	_ = a.closeOpen()
	if len(a.blocks) != 2 || a.blocks[0].Type != "text" || a.blocks[1].Type != "tool_use" {
		t.Errorf("blocks = %+v", a.blocks)
	}
}

func TestBlockAssemblerSSEEvents(t *testing.T) {
	var events []string
	emit := func(event string, data any) error {
		events = append(events, event)
		return nil
	}
	a := newBlockAssembler(emit)
	_ = a.addText("hi")
	_ = a.closeOpen()
	want := []string{"content_block_start", "content_block_delta", "content_block_stop"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}

func TestBlockAssemblerToolUseSSE(t *testing.T) {
	type rec struct {
		name string
		data map[string]any
	}
	var got []rec
	emit := func(name string, data any) error {
		got = append(got, rec{name, data.(map[string]any)})
		return nil
	}
	a := newBlockAssembler(emit)
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1", ToolName: "get_weather",
		ToolInput: `{"city":"NYC"}`, ToolStop: true})

	if len(got) != 3 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].name != "content_block_start" {
		t.Errorf("event[0] = %q", got[0].name)
	}
	cb, _ := got[0].data["content_block"].(map[string]any)
	if cb == nil || cb["type"] != "tool_use" || cb["id"] != "t1" || cb["name"] != "get_weather" {
		t.Errorf("content_block_start payload = %+v", got[0].data)
	}
	if got[1].name != "content_block_delta" {
		t.Errorf("event[1] = %q", got[1].name)
	}
	delta, _ := got[1].data["delta"].(map[string]any)
	if delta == nil || delta["type"] != "input_json_delta" || delta["partial_json"] != `{"city":"NYC"}` {
		t.Errorf("delta payload = %+v", got[1].data)
	}
	if got[2].name != "content_block_stop" {
		t.Errorf("event[2] = %q", got[2].name)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := map[int]int{0: 0, -5: 0, 1: 1, 4: 1, 5: 2, 8: 2, 9: 3}
	for in, want := range cases {
		if got := estimateTokens(in); got != want {
			t.Errorf("estimateTokens(%d) = %d, want %d", in, got, want)
		}
	}
}

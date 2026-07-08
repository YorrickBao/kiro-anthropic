package main

import (
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		assert.Equalf(t, want, mapModel(in), "mapModel(%q)", in)
	}
}

func TestMediaTypeToKiroImageFormat(t *testing.T) {
	ok := map[string]string{
		"image/png": "png", "image/jpeg": "jpeg", "image/jpg": "jpeg",
		"image/gif": "gif", "image/webp": "webp", "IMAGE/PNG": "png", " image/png ": "png",
	}
	for mt, want := range ok {
		got, valid := mediaTypeToKiroImageFormat(mt)
		assert.Truef(t, valid, "mediaTypeToKiroImageFormat(%q) should be valid", mt)
		assert.Equalf(t, want, got, "mediaTypeToKiroImageFormat(%q)", mt)
	}
	for _, mt := range []string{"", "image/svg+xml", "text/plain"} {
		_, valid := mediaTypeToKiroImageFormat(mt)
		assert.Falsef(t, valid, "mediaTypeToKiroImageFormat(%q) should be invalid", mt)
	}
}

func TestConvertImage(t *testing.T) {
	b := anthropicContentBlock{Type: "image", Source: &anthropicImageSource{
		Type: "base64", MediaType: "image/png", Data: "QUJD"}}
	img, ok := convertImage(b)
	require.True(t, ok)
	assert.Equal(t, "png", img.Format)
	assert.Equal(t, "QUJD", img.Source.Bytes)

	// url source (no data) -> skipped
	_, ok = convertImage(anthropicContentBlock{Type: "image",
		Source: &anthropicImageSource{Type: "url", URL: "http://x/y.png"}})
	assert.False(t, ok, "url image should be skipped")

	// unsupported media type -> skipped
	_, ok = convertImage(anthropicContentBlock{Type: "image",
		Source: &anthropicImageSource{Type: "base64", MediaType: "image/svg+xml", Data: "QQ=="}})
	assert.False(t, ok, "svg image should be skipped")
}

func TestRequestedEffort(t *testing.T) {
	assert.Equal(t, "high", requestedEffort(&anthropicRequest{
		OutputConfig: &anthropicOutputConfig{Effort: "high"}, ReasoningEffort: "low"}),
		"output_config.effort should win")
	assert.Equal(t, "medium", requestedEffort(&anthropicRequest{ReasoningEffort: "medium"}),
		"reasoning_effort fallback")
	assert.Equal(t, "", requestedEffort(&anthropicRequest{}))
}

func TestNormalizeToolInput(t *testing.T) {
	assert.Equal(t, `{}`, string(normalizeToolInput("")), "empty should be {}")
	assert.Equal(t, `{"a":1}`, string(normalizeToolInput(`{"a":1}`)), "valid json should pass through")

	got := normalizeToolInput("not json")
	var m map[string]string
	require.NoError(t, json.Unmarshal(got, &m))
	assert.Equal(t, "not json", m["_raw"], "invalid json should be wrapped")
}

func TestExtractText(t *testing.T) {
	assert.Equal(t, "hi", extractText(json.RawMessage(`"hi"`)))
	assert.Equal(t, "ab", extractText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)))
	assert.Equal(t, "", extractText(nil))
}

func TestConvertToolResult(t *testing.T) {
	// string content
	tr := convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t1",
		Content: json.RawMessage(`"result text"`)})
	assert.Equal(t, "t1", tr.ToolUseID)
	assert.Equal(t, "success", tr.Status)
	require.Len(t, tr.Content, 1)
	assert.Equal(t, "result text", tr.Content[0].Text)

	// error flag
	tr = convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t2",
		IsError: true, Content: json.RawMessage(`"boom"`)})
	assert.Equal(t, "error", tr.Status, "is_error should map to error status")

	// array of text blocks
	tr = convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t3",
		Content: json.RawMessage(`[{"type":"text","text":"x"}]`)})
	require.Len(t, tr.Content, 1)
	assert.Equal(t, "x", tr.Content[0].Text)

	// array with a non-text block -> preserved as JSON, not dropped
	tr = convertToolResult(anthropicContentBlock{Type: "tool_result", ToolUseID: "t4",
		Content: json.RawMessage(`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QQ=="}}]`)})
	require.Len(t, tr.Content, 1)
	assert.Empty(t, tr.Content[0].Text)
	assert.NotEmpty(t, tr.Content[0].JSON)
	assert.Contains(t, string(tr.Content[0].JSON), "image")
}

func TestBuildKiroRequestBasic(t *testing.T) {
	areq := &anthropicRequest{
		Model:    "claude-opus-4.8",
		System:   json.RawMessage(`"you are helpful"`),
		Messages: []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}
	k, err := buildKiroRequest(&Config{AgentMode: "vibe"}, areq)
	require.NoError(t, err)
	assert.Equal(t, "MANUAL", k.ConversationState.ChatTriggerType)
	assert.Equal(t, "you are helpful", k.SystemPrompt)
	assert.Equal(t, "vibe", k.AgentMode)

	um := k.ConversationState.CurrentMessage.UserInputMessage
	require.NotNil(t, um)
	assert.Equal(t, "hello", um.Content)
	assert.Equal(t, "claude-opus-4.8", um.ModelID)
	assert.Equal(t, "AI_EDITOR", um.Origin)
	assert.Empty(t, k.ConversationState.History, "history should be empty")
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
	require.NoError(t, err)

	// current = last user (tool_result)
	cur := k.ConversationState.CurrentMessage.UserInputMessage
	require.NotNil(t, cur)
	require.NotNil(t, cur.UserInputMessageContext)
	require.Len(t, cur.UserInputMessageContext.ToolResults, 1)
	assert.Equal(t, "tu1", cur.UserInputMessageContext.ToolResults[0].ToolUseID)

	// tools attached to current message
	require.Len(t, cur.UserInputMessageContext.Tools, 1)
	assert.Equal(t, "get_weather", cur.UserInputMessageContext.Tools[0].ToolSpecification.Name)

	// history: [user, assistant] and starts with user
	require.Len(t, k.ConversationState.History, 2)
	assert.NotNil(t, k.ConversationState.History[0].UserInputMessage, "history should start with a user turn")

	am := k.ConversationState.History[1].AssistantResponseMessage
	require.NotNil(t, am)
	require.Len(t, am.ToolUses, 1)
	assert.Equal(t, "tu1", am.ToolUses[0].ToolUseID)
}

func TestBuildKiroRequestImage(t *testing.T) {
	areq := &anthropicRequest{
		Model: "claude-opus-4.8",
		Messages: []anthropicMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"what is this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]`)}},
	}
	k, err := buildKiroRequest(&Config{}, areq)
	require.NoError(t, err)
	um := k.ConversationState.CurrentMessage.UserInputMessage
	require.Len(t, um.Images, 1)
	assert.Equal(t, "png", um.Images[0].Format)
	assert.Equal(t, "QUJD", um.Images[0].Source.Bytes)
}

func TestBuildKiroRequestEmptyMessages(t *testing.T) {
	_, err := buildKiroRequest(&Config{}, &anthropicRequest{})
	assert.Error(t, err, "expected error for empty messages")
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
	require.NoError(t, err)

	cur := k.ConversationState.CurrentMessage.UserInputMessage
	require.NotNil(t, cur)
	assert.Equal(t, " ", cur.Content)
	assert.Equal(t, "claude-opus-4.8", cur.ModelID)

	require.Len(t, k.ConversationState.History, 2)
	assert.NotNil(t, k.ConversationState.History[0].UserInputMessage, "history[0] should be the user turn")
	am := k.ConversationState.History[1].AssistantResponseMessage
	require.NotNil(t, am)
	assert.Equal(t, "partial answer", am.Content)
}

// --- blockAssembler ---

func TestBlockAssemblerText(t *testing.T) {
	a := newBlockAssembler(nil)
	_ = a.addText("Hello")
	_ = a.addText(" world")
	_ = a.closeOpen()
	require.Len(t, a.blocks, 1)
	assert.Equal(t, "text", a.blocks[0].Type)
	assert.Equal(t, "Hello world", a.blocks[0].Text)
	assert.Equal(t, "end_turn", a.stopReason())
}

func TestBlockAssemblerToolUse(t *testing.T) {
	a := newBlockAssembler(nil)
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1", ToolName: "get_weather", ToolInput: `{"city":`})
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1", ToolInput: `"NYC"}`, ToolStop: true})
	require.Len(t, a.blocks, 1)
	b := a.blocks[0]
	assert.Equal(t, "tool_use", b.Type)
	assert.Equal(t, "t1", b.ID)
	assert.Equal(t, "get_weather", b.Name)
	assert.Equal(t, `{"city":"NYC"}`, string(b.Input))
	assert.Equal(t, "tool_use", a.stopReason())
}

func TestBlockAssemblerTextThenTool(t *testing.T) {
	a := newBlockAssembler(nil)
	_ = a.addText("Answer:")
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t9", ToolName: "f", ToolInput: `{}`, ToolStop: true})
	_ = a.closeOpen()
	require.Len(t, a.blocks, 2)
	assert.Equal(t, "text", a.blocks[0].Type)
	assert.Equal(t, "tool_use", a.blocks[1].Type)
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
	assert.Equal(t, []string{"content_block_start", "content_block_delta", "content_block_stop"}, events)
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

	require.Len(t, got, 3)
	assert.Equal(t, "content_block_start", got[0].name)
	cb, _ := got[0].data["content_block"].(map[string]any)
	require.NotNil(t, cb)
	assert.Equal(t, "tool_use", cb["type"])
	assert.Equal(t, "t1", cb["id"])
	assert.Equal(t, "get_weather", cb["name"])

	assert.Equal(t, "content_block_delta", got[1].name)
	delta, _ := got[1].data["delta"].(map[string]any)
	require.NotNil(t, delta)
	assert.Equal(t, "input_json_delta", delta["type"])
	assert.Equal(t, `{"city":"NYC"}`, delta["partial_json"])

	assert.Equal(t, "content_block_stop", got[2].name)
}

func TestEstimateTokens(t *testing.T) {
	cases := map[int]int{0: 0, -5: 0, 1: 1, 4: 1, 5: 2, 8: 2, 9: 3}
	for in, want := range cases {
		assert.Equalf(t, want, estimateTokens(in), "estimateTokens(%d)", in)
	}
}

// --- extended thinking / reasoning ---

func TestRequestedEffortThinking(t *testing.T) {
	// thinking disabled -> minimize sentinel
	assert.Equal(t, effortMinimize, requestedEffort(&anthropicRequest{Thinking: &anthropicThinking{Type: "disabled"}}),
		"disabled thinking -> minimize sentinel")
	// thinking enabled -> default (empty = top out)
	assert.Equal(t, "", requestedEffort(&anthropicRequest{Thinking: &anthropicThinking{Type: "enabled", BudgetTokens: 5000}}),
		"enabled thinking -> empty")
	// explicit effort still wins over the thinking toggle
	assert.Equal(t, "high", requestedEffort(&anthropicRequest{
		OutputConfig: &anthropicOutputConfig{Effort: "high"},
		Thinking:     &anthropicThinking{Type: "disabled"}}),
		"explicit effort should win")
	// suppression flag
	assert.True(t, thinkingSuppressed(&anthropicRequest{Thinking: &anthropicThinking{Type: "disabled"}}),
		"disabled thinking should be suppressed")
	assert.False(t, thinkingSuppressed(&anthropicRequest{Thinking: &anthropicThinking{Type: "enabled"}}),
		"enabled thinking should not be suppressed")
	assert.False(t, thinkingSuppressed(&anthropicRequest{}),
		"absent thinking should not be suppressed")
}

func TestBlockAssemblerReasoning(t *testing.T) {
	a := newBlockAssembler(nil)
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningText: "let me "})
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningText: "think"})
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningSignature: "SIG=="})
	_ = a.addText("answer")
	_ = a.closeOpen()
	require.Len(t, a.blocks, 2)
	assert.Equal(t, "thinking", a.blocks[0].Type)
	assert.Equal(t, "let me think", a.blocks[0].Thinking)
	assert.Equal(t, "SIG==", a.blocks[0].Signature)
	assert.Equal(t, "text", a.blocks[1].Type)
	assert.Equal(t, "answer", a.blocks[1].Text)
	// thinking precedes text and is not a tool turn
	assert.Equal(t, "end_turn", a.stopReason())
}

func TestBlockAssemblerStopReason(t *testing.T) {
	// 1. tool_use inferred from sawToolUse when no authoritative reason.
	a := newBlockAssembler(nil)
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t", ToolName: "f"})
	_ = a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t", ToolStop: true})
	_ = a.closeOpen()
	assert.Equal(t, "tool_use", a.stopReason(), "inferred tool_use")

	// 2. authoritative metadataEvent.stopReason wins over the guess.
	a.setStopReason("MAX_TOKENS")
	assert.Equal(t, "max_tokens", a.stopReason(), "authoritative should win")

	// 3. unknown backend value is ignored -> fallback still applies.
	a2 := newBlockAssembler(nil)
	_ = a2.addText("hi")
	_ = a2.closeOpen()
	a2.setStopReason("WHATEVER")
	assert.Equal(t, "end_turn", a2.stopReason(), "unknown reason should fall back")
}

func TestBlockAssemblerReasoningSSE(t *testing.T) {
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
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningText: "hmm"})
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningSignature: "SIG=="})
	_ = a.closeOpen()
	// content_block_start(thinking) -> thinking_delta -> signature_delta -> stop
	require.Len(t, got, 4)
	cb := got[0].data["content_block"].(map[string]any)
	assert.Equal(t, "content_block_start", got[0].name)
	assert.Equal(t, "thinking", cb["type"])

	d1 := got[1].data["delta"].(map[string]any)
	assert.Equal(t, "content_block_delta", got[1].name)
	assert.Equal(t, "thinking_delta", d1["type"])
	assert.Equal(t, "hmm", d1["thinking"])

	d2 := got[2].data["delta"].(map[string]any)
	assert.Equal(t, "content_block_delta", got[2].name)
	assert.Equal(t, "signature_delta", d2["type"])
	assert.Equal(t, "SIG==", d2["signature"])

	assert.Equal(t, "content_block_stop", got[3].name)
}

func TestBlockAssemblerReasoningSuppressed(t *testing.T) {
	a := newBlockAssembler(nil)
	a.emitThinking = false
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningText: "secret"})
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningSignature: "SIG=="})
	_ = a.addText("answer")
	_ = a.closeOpen()
	require.Len(t, a.blocks, 1)
	assert.Equal(t, "text", a.blocks[0].Type)
}

func TestBlockAssemblerRedactedThinking(t *testing.T) {
	var events []string
	emit := func(name string, _ any) error { events = append(events, name); return nil }
	a := newBlockAssembler(emit)
	_ = a.addReasoning(&kiroEvent{Kind: evReasoning, ReasoningRedacted: "REDACTED=="})
	_ = a.closeOpen()
	require.Len(t, a.blocks, 1)
	assert.Equal(t, "redacted_thinking", a.blocks[0].Type)
	assert.Equal(t, "REDACTED==", a.blocks[0].Data)
	// redacted reasoning has no delta: start (carrying data) then stop.
	assert.Equal(t, []string{"content_block_start", "content_block_stop"}, events)
}

func TestBuildKiroRequestThinkingHistory(t *testing.T) {
	areq := &anthropicRequest{
		Model: "claude-opus-4.8",
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"solve it"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"thinking","thinking":"reasoning...","signature":"SIG=="},{"type":"text","text":"the answer"}]`)},
			{Role: "user", Content: json.RawMessage(`"thanks"`)},
		},
	}
	k, err := buildKiroRequest(&Config{}, areq)
	require.NoError(t, err)
	am := k.ConversationState.History[1].AssistantResponseMessage
	require.NotNil(t, am)
	require.NotNil(t, am.ReasoningContent)
	require.NotNil(t, am.ReasoningContent.ReasoningText)
	assert.Equal(t, "reasoning...", am.ReasoningContent.ReasoningText.Text)
	assert.Equal(t, "SIG==", am.ReasoningContent.ReasoningText.Signature)
	assert.Equal(t, "the answer", am.Content)

	// the round-tripped reasoningContent must serialize under the union shape.
	raw, _ := json.Marshal(am)
	assert.Contains(t, string(raw), `"reasoningContent":{"reasoningText":{"text":"reasoning...","signature":"SIG=="}}`)
}

func TestBuildKiroRequestRedactedThinkingHistory(t *testing.T) {
	areq := &anthropicRequest{
		Model: "claude-opus-4.8",
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"redacted_thinking","data":"ENC=="},{"type":"text","text":"a"}]`)},
			{Role: "user", Content: json.RawMessage(`"more"`)},
		},
	}
	k, err := buildKiroRequest(&Config{}, areq)
	require.NoError(t, err)
	am := k.ConversationState.History[1].AssistantResponseMessage
	require.NotNil(t, am)
	require.NotNil(t, am.ReasoningContent)
	assert.Equal(t, "ENC==", am.ReasoningContent.RedactedContent)
}

func TestStripReasoningFromHistory(t *testing.T) {
	kreq := &kiroRequest{ConversationState: kiroConversationState{History: []kiroMessage{
		{UserInputMessage: &kiroUserInputMessage{Content: "hi"}},
		{AssistantResponseMessage: &kiroAssistantMessage{Content: "a",
			ReasoningContent: &kiroReasoningContent{ReasoningText: &kiroReasoningText{Text: "t", Signature: "s"}}}},
	}}}
	assert.True(t, stripReasoningFromHistory(kreq), "should report stripped")
	assert.Nil(t, kreq.ConversationState.History[1].AssistantResponseMessage.ReasoningContent,
		"reasoningContent not stripped")
	// nothing left to strip -> false
	assert.False(t, stripReasoningFromHistory(kreq), "second strip should be false")
}

// --- tool-call marker leak fix (strip-only) ---

func firstText(blocks []anthropicRespBlock) string {
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

func countToolUse(blocks []anthropicRespBlock) int {
	n := 0
	for _, b := range blocks {
		if b.Type == "tool_use" {
			n++
		}
	}
	return n
}

// deepseek leaks only the opening marker as trailing text; the tool itself
// arrives structured. The marker must be stripped, the tool kept.
func TestToolLeakDeepSeekTrailingMarker(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText("I'll get the weather.\n\n<｜DSML｜function_calls"))
	require.NoError(t, a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1",
		ToolName: "get_weather", ToolInput: `{"city":"Paris"}`, ToolStop: true}))
	require.NoError(t, a.finish())

	txt := firstText(a.blocks)
	assert.NotContains(t, txt, "DSML", "marker must be stripped")
	assert.NotContains(t, txt, "function_calls")
	assert.Equal(t, "I'll get the weather.\n\n", txt)
	assert.Equal(t, 1, countToolUse(a.blocks))
	assert.Equal(t, "tool_use", a.stopReason())
}

// The opening marker split across frames must still be stripped.
func TestToolLeakCrossFrameSplitMarker(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText("Answer.\n\n<｜DSML｜function_c"))
	require.NoError(t, a.ingestText("alls"))
	require.NoError(t, a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1",
		ToolName: "f", ToolInput: `{}`, ToolStop: true}))
	require.NoError(t, a.finish())

	txt := firstText(a.blocks)
	assert.NotContains(t, txt, "DSML")
	assert.NotContains(t, txt, "function_c")
	assert.Equal(t, "Answer.\n\n", txt)
	assert.Equal(t, 1, countToolUse(a.blocks))
}

// The Anthropic-style "<function_calls>" opener is stripped too.
func TestToolLeakAnthropicMarker(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText("Done.\n<function_calls>"))
	require.NoError(t, a.finish())
	assert.Equal(t, "Done.\n", firstText(a.blocks))
}

// A frame boundary landing inside the multibyte "｜" (U+FF5C, 3 bytes) of the
// DSML marker must not split the rune in an emitted delta, and the marker must
// still be stripped once completed.
func TestToolLeakSplitInsideMultibyteRune(t *testing.T) {
	var deltas []string
	emit := func(event string, data any) error {
		if event == "content_block_delta" {
			m := data.(map[string]any)
			if d, ok := m["delta"].(map[string]any); ok && d["type"] == "text_delta" {
				deltas = append(deltas, d["text"].(string))
			}
		}
		return nil
	}
	a := newBlockAssembler(emit)
	// Split the marker mid-"｜": "<\xef" ends the first frame, "\xbd\x9c..." the next.
	full := "Hi.\n\n" + dsmlFunctionCalls
	mid := len("Hi.\n\n<") + 1 // one byte into the first ｜
	require.NoError(t, a.ingestText(full[:mid]))
	require.NoError(t, a.ingestText(full[mid:]))
	require.NoError(t, a.finish())

	for _, d := range deltas {
		assert.True(t, utf8.ValidString(d), "each delta must be valid UTF-8: %q", d)
	}
	assert.Equal(t, "Hi.\n\n", firstText(a.blocks))
}

// A dangling marker with no following tool (pure leak) is stripped at flush.
func TestToolLeakTrailingMarkerOnlyStripped(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText("Here you go.\n\n<｜DSML｜function_calls"))
	require.NoError(t, a.finish())
	assert.Equal(t, "Here you go.\n\n", firstText(a.blocks))
	assert.Equal(t, 0, countToolUse(a.blocks))
}

// Leaked tool-call BODY is NOT parsed into a phantom tool: strip-only leaves the
// markup as text and never invents a tool_use. (No parse/rescue by design.)
func TestToolLeakBodyNotParsedIntoPhantomTool(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText(`x <invoke name="f"><parameter name="a">1</parameter></invoke>`))
	require.NoError(t, a.finish())
	assert.Equal(t, 0, countToolUse(a.blocks), "no phantom tool synthesized")
	assert.Contains(t, firstText(a.blocks), "<invoke", "body kept as text")
}

// Legitimate text ending in "count" must not be eaten (no count-corruption rule).
func TestToolLeakCountNotEaten(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText("The final word count"))
	require.NoError(t, a.finish())
	assert.Equal(t, "The final word count", firstText(a.blocks))
	assert.Equal(t, 0, countToolUse(a.blocks))
}

// Prose mentioning "function" or a lone '<' is untouched, and a mid-text
// "<function_calls>" (not at the end) is not stripped.
func TestToolLeakNoFalsePositiveOnProse(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText("Use the function foo() when a < b holds"))
	require.NoError(t, a.finish())
	assert.Equal(t, "Use the function foo() when a < b holds", firstText(a.blocks))

	b := newBlockAssembler(nil)
	require.NoError(t, b.ingestText("The <function_calls> tag opens a call."))
	require.NoError(t, b.finish())
	assert.Equal(t, "The <function_calls> tag opens a call.", firstText(b.blocks))
}

// A held trailing fragment that turns out to be normal text is emitted intact
// once more text arrives (no data loss from cross-frame hold).
func TestToolLeakHeldFragmentReleased(t *testing.T) {
	a := newBlockAssembler(nil)
	require.NoError(t, a.ingestText("ends with <")) // '<' could start a marker
	require.NoError(t, a.ingestText("b so a<b"))    // ...but it was just prose
	require.NoError(t, a.finish())
	assert.Equal(t, "ends with <b so a<b", firstText(a.blocks))
}

// Streaming: no text_delta ever carries the leaked marker, and the structured
// tool block is still emitted.
func TestToolLeakStreamingNoMarkerInDeltas(t *testing.T) {
	var textDeltas []string
	var sawToolStart bool
	emit := func(event string, data any) error {
		m := data.(map[string]any)
		if event == "content_block_delta" {
			if d, ok := m["delta"].(map[string]any); ok && d["type"] == "text_delta" {
				textDeltas = append(textDeltas, d["text"].(string))
			}
		}
		if event == "content_block_start" {
			if cb, ok := m["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				sawToolStart = true
			}
		}
		return nil
	}
	a := newBlockAssembler(emit)
	require.NoError(t, a.ingestText("Weather:\n\n<｜DSML｜function_c"))
	require.NoError(t, a.ingestText("alls"))
	require.NoError(t, a.addToolUse(&kiroEvent{Kind: evToolUse, ToolUseID: "t1",
		ToolName: "get_weather", ToolInput: `{"city":"Paris"}`, ToolStop: true}))
	require.NoError(t, a.finish())

	joined := ""
	for _, d := range textDeltas {
		joined += d
	}
	assert.NotContains(t, joined, "DSML", "no leaked marker in any text_delta")
	assert.NotContains(t, joined, "function_c")
	assert.Equal(t, "Weather:\n\n", joined)
	assert.True(t, sawToolStart, "structured tool block still emitted")
}

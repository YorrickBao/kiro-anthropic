package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- shared model fixtures (reused by server_test.go) ---

// mirrors the real opus-4.8 additionalModelRequestFieldsSchema.
const testOpusSchema = `{"properties":{"output_config":{"properties":{"effort":{"enum":["low","medium","high","xhigh","max"]}}},"max_tokens":{"minimum":1024,"maximum":128000}}}`

const testReasoningSchema = `{"properties":{"reasoning":{"properties":{"effort":{"enum":["low","high"]}}}}}`

func testOpusModel() kiroModelInfo {
	m := kiroModelInfo{ModelID: "claude-opus-4.8", ModelName: "Claude Opus 4.8",
		AdditionalModelRequestFieldsSchema: json.RawMessage(testOpusSchema)}
	m.TokenLimits.MaxInputTokens = 1000000
	m.TokenLimits.MaxOutputTokens = 128000
	m.RateMultiplier = 1.5
	m.RateUnit = "credit"
	return m
}

func testSonnet45Model() kiroModelInfo {
	m := kiroModelInfo{ModelID: "claude-sonnet-4.5", ModelName: "Claude Sonnet 4.5"}
	m.TokenLimits.MaxInputTokens = 200000
	m.TokenLimits.MaxOutputTokens = 64000
	return m
}

func TestModelEffort(t *testing.T) {
	ec, ok := testOpusModel().effort()
	require.True(t, ok)
	assert.Equal(t, "output_config", ec.SchemaPath)
	require.Len(t, ec.Levels, 5)
	assert.Equal(t, "low", ec.Levels[0])
	assert.Equal(t, "max", ec.max())

	_, ok = testSonnet45Model().effort()
	assert.False(t, ok, "sonnet-4.5 should not support effort")

	// "reasoning" schema path variant
	m := kiroModelInfo{AdditionalModelRequestFieldsSchema: json.RawMessage(testReasoningSchema)}
	ec, ok = m.effort()
	require.True(t, ok)
	assert.Equal(t, "reasoning", ec.SchemaPath)
	assert.Len(t, ec.Levels, 2)
}

func TestModelMaxTokensRange(t *testing.T) {
	lo, hi, ok := testOpusModel().maxTokensRange()
	require.True(t, ok)
	assert.Equal(t, 1024, lo)
	assert.Equal(t, 128000, hi)

	_, _, ok = testSonnet45Model().maxTokensRange()
	assert.False(t, ok, "sonnet-4.5 should have no max_tokens field")

	// max_tokens declared without an explicit maximum -> fall back to tokenLimits.
	m := kiroModelInfo{AdditionalModelRequestFieldsSchema: json.RawMessage(`{"properties":{"max_tokens":{}}}`)}
	m.TokenLimits.MaxOutputTokens = 8192
	lo, hi, ok = m.maxTokensRange()
	require.True(t, ok)
	assert.Equal(t, 1, lo)
	assert.Equal(t, 8192, hi)
}

func TestEffortConfigClamp(t *testing.T) {
	ec := effortConfig{Levels: []string{"low", "medium", "high", "max"}}
	assert.True(t, ec.has("max"))
	assert.False(t, ec.has("xhigh"))
	assert.Equal(t, "max", ec.max())
	assert.Equal(t, "low", ec.clamp("low"), "clamp(low) should stay low")
	assert.Equal(t, "max", ec.clamp("xhigh"), "clamp(xhigh) should top out at max")
	assert.Empty(t, (effortConfig{}).max(), "empty effortConfig.max() should be empty")
}

func TestParseKiroMessage(t *testing.T) {
	// assistant text
	ev := parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "assistantResponseEvent"},
		payload: []byte(`{"content":"hi"}`)})
	assert.Equal(t, evText, ev.Kind)
	assert.Equal(t, "hi", ev.Text)

	// tool use
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "toolUseEvent"},
		payload: []byte(`{"toolUseId":"t1","name":"f","input":"{\"a\":1}","stop":true}`)})
	assert.Equal(t, evToolUse, ev.Kind)
	assert.Equal(t, "t1", ev.ToolUseID)
	assert.Equal(t, "f", ev.ToolName)
	assert.Equal(t, `{"a":1}`, ev.ToolInput)
	assert.True(t, ev.ToolStop)

	// metadata (terminal frame: authoritative stopReason + conversationId)
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "metadataEvent"},
		payload: []byte(`{"stopReason":"TOOL_USE","conversationId":"c1"}`)})
	assert.Equal(t, evMetadata, ev.Kind)
	assert.Equal(t, "c1", ev.ConversationID)
	assert.Equal(t, "TOOL_USE", ev.StopReason)

	// exception via message-type header
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":message-type": "exception", ":exception-type": "ThrottlingException"},
		payload: []byte(`{"message":"slow down"}`)})
	assert.Equal(t, evError, ev.Kind)
	assert.Equal(t, "ThrottlingException", ev.ErrKind)
	assert.Equal(t, "slow down", ev.ErrMsg)

	// unknown event -> other
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "codeReferenceEvent"}, payload: []byte(`{}`)})
	assert.Equal(t, evOther, ev.Kind)
}

func TestRawToInputString(t *testing.T) {
	assert.Equal(t, "abc", rawToInputString(json.RawMessage(`"abc"`)))
	assert.Equal(t, `{"a":1}`, rawToInputString(json.RawMessage(`{"a":1}`)))
	assert.Equal(t, "", rawToInputString(nil))
}

func TestExtractMessageAndReason(t *testing.T) {
	msg, reason := extractMessageAndReason([]byte(`{"message":"x"}`))
	assert.Equal(t, "x", msg)
	assert.Empty(t, reason)

	msg, _ = extractMessageAndReason([]byte(`{"Message":"y"}`))
	assert.Equal(t, "y", msg)

	msg, _ = extractMessageAndReason([]byte(`plain`))
	assert.Equal(t, "plain", msg)

	msg, reason = extractMessageAndReason([]byte(`{"message":"bad sig","reason":"THINKING_SIGNATURE_INVALID"}`))
	assert.Equal(t, "bad sig", msg)
	assert.Equal(t, "THINKING_SIGNATURE_INVALID", reason)
}

func TestKiroHTTPErrorReason(t *testing.T) {
	e := &kiroHTTPError{Status: 400, Body: `{"message":"m","reason":"THINKING_SIGNATURE_INVALID"}`}
	assert.Equal(t, "THINKING_SIGNATURE_INVALID", e.reason())
	assert.Empty(t, (&kiroHTTPError{Body: "not json"}).reason(), "non-json reason should be empty")
}

func TestParseReasoningContentEvent(t *testing.T) {
	// text chunk
	ev := parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "reasoningContentEvent"},
		payload: []byte(`{"text":"let me think"}`)})
	assert.Equal(t, evReasoning, ev.Kind)
	assert.Equal(t, "let me think", ev.ReasoningText)
	assert.Empty(t, ev.ReasoningSignature)

	// signature-only frame
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "reasoningContentEvent"},
		payload: []byte(`{"signature":"SIG=="}`)})
	assert.Equal(t, evReasoning, ev.Kind)
	assert.Empty(t, ev.ReasoningText)
	assert.Equal(t, "SIG==", ev.ReasoningSignature)
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"END_TURN":      "end_turn",
		"end_turn":      "end_turn", // case-insensitive
		"TOOL_USE":      "tool_use",
		"MAX_TOKENS":    "max_tokens",
		"STOP_SEQUENCE": "stop_sequence",
		"  tool_use  ":  "tool_use", // trimmed
		"":              "",
		"BOGUS":         "",
	}
	for in, want := range cases {
		assert.Equalf(t, want, mapStopReason(in), "mapStopReason(%q)", in)
	}
}

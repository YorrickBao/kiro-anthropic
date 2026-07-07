package main

import (
	"encoding/json"
	"testing"
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
	if !ok || ec.SchemaPath != "output_config" {
		t.Fatalf("opus effort = %+v,%v", ec, ok)
	}
	if len(ec.Levels) != 5 || ec.Levels[0] != "low" || ec.max() != "max" {
		t.Errorf("opus levels = %v", ec.Levels)
	}

	if _, ok := testSonnet45Model().effort(); ok {
		t.Errorf("sonnet-4.5 should not support effort")
	}

	// "reasoning" schema path variant
	m := kiroModelInfo{AdditionalModelRequestFieldsSchema: json.RawMessage(testReasoningSchema)}
	ec, ok = m.effort()
	if !ok || ec.SchemaPath != "reasoning" || len(ec.Levels) != 2 {
		t.Errorf("reasoning effort = %+v,%v", ec, ok)
	}
}

func TestModelMaxTokensRange(t *testing.T) {
	lo, hi, ok := testOpusModel().maxTokensRange()
	if !ok || lo != 1024 || hi != 128000 {
		t.Errorf("opus maxTokensRange = %d,%d,%v", lo, hi, ok)
	}
	if _, _, ok := testSonnet45Model().maxTokensRange(); ok {
		t.Errorf("sonnet-4.5 should have no max_tokens field")
	}
	// max_tokens declared without an explicit maximum -> fall back to tokenLimits.
	m := kiroModelInfo{AdditionalModelRequestFieldsSchema: json.RawMessage(`{"properties":{"max_tokens":{}}}`)}
	m.TokenLimits.MaxOutputTokens = 8192
	lo, hi, ok = m.maxTokensRange()
	if !ok || lo != 1 || hi != 8192 {
		t.Errorf("fallback maxTokensRange = %d,%d,%v", lo, hi, ok)
	}
}

func TestEffortConfigClamp(t *testing.T) {
	ec := effortConfig{Levels: []string{"low", "medium", "high", "max"}}
	if !ec.has("max") || ec.has("xhigh") {
		t.Errorf("has() wrong")
	}
	if ec.max() != "max" {
		t.Errorf("max() = %q", ec.max())
	}
	if ec.clamp("low") != "low" {
		t.Errorf("clamp(low) should stay low")
	}
	if ec.clamp("xhigh") != "max" {
		t.Errorf("clamp(xhigh) should top out at max, got %q", ec.clamp("xhigh"))
	}
	if (effortConfig{}).max() != "" {
		t.Errorf("empty effortConfig.max() should be empty")
	}
}

func TestParseKiroMessage(t *testing.T) {
	// assistant text
	ev := parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "assistantResponseEvent"},
		payload: []byte(`{"content":"hi"}`)})
	if ev.Kind != evText || ev.Text != "hi" {
		t.Errorf("assistantResponseEvent = %+v", ev)
	}

	// tool use
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "toolUseEvent"},
		payload: []byte(`{"toolUseId":"t1","name":"f","input":"{\"a\":1}","stop":true}`)})
	if ev.Kind != evToolUse || ev.ToolUseID != "t1" || ev.ToolName != "f" || ev.ToolInput != `{"a":1}` || !ev.ToolStop {
		t.Errorf("toolUseEvent = %+v", ev)
	}

	// metadata
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "messageMetadataEvent"},
		payload: []byte(`{"conversationId":"c1"}`)})
	if ev.Kind != evMetadata || ev.ConversationID != "c1" {
		t.Errorf("messageMetadataEvent = %+v", ev)
	}

	// exception via message-type header
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":message-type": "exception", ":exception-type": "ThrottlingException"},
		payload: []byte(`{"message":"slow down"}`)})
	if ev.Kind != evError || ev.ErrKind != "ThrottlingException" || ev.ErrMsg != "slow down" {
		t.Errorf("exception = %+v", ev)
	}

	// unknown event -> other
	ev = parseKiroMessage(&eventMessage{
		headers: map[string]string{":event-type": "codeReferenceEvent"}, payload: []byte(`{}`)})
	if ev.Kind != evOther {
		t.Errorf("unknown event kind = %v", ev.Kind)
	}
}

func TestRawToInputString(t *testing.T) {
	if got := rawToInputString(json.RawMessage(`"abc"`)); got != "abc" {
		t.Errorf("json string = %q", got)
	}
	if got := rawToInputString(json.RawMessage(`{"a":1}`)); got != `{"a":1}` {
		t.Errorf("raw object = %q", got)
	}
	if got := rawToInputString(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestExtractMessage(t *testing.T) {
	if got := extractMessage([]byte(`{"message":"x"}`)); got != "x" {
		t.Errorf("message = %q", got)
	}
	if got := extractMessage([]byte(`{"Message":"y"}`)); got != "y" {
		t.Errorf("Message = %q", got)
	}
	if got := extractMessage([]byte(`plain`)); got != "plain" {
		t.Errorf("plain = %q", got)
	}
}

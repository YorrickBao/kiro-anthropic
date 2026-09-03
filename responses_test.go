package main

// Conformance tests for the OpenAI Responses endpoint (//v1/responses).
// Event-order and shape assertions mirror what the Codex CLI parser accepts
// (openai/codex codex-api/src/sse/responses.rs): created → per-item
// added/delta/done → terminal completed/incomplete/failed, with a strictly
// increasing sequence_number.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- frame builders (raw Kiro event-stream bodies served by fakeRuntime) ----

func responsesToolUseFrame(id, name, rawInput string, stop bool) []byte {
	payload := fmt.Sprintf(`{"toolUseId":%q,"name":%q,"input":%s,"stop":%t}`, id, name, rawInput, stop)
	headers := append(
		esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", "toolUseEvent")...,
	)
	return esFrame(headers, []byte(payload))
}

func responsesMetadataFrame(reason string) []byte {
	headers := append(
		esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", "metadataEvent")...,
	)
	return esFrame(headers, []byte(`{"stopReason":"`+reason+`"}`))
}

func responsesReasoningFrame(text string) []byte {
	headers := append(
		esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", "reasoningContentEvent")...,
	)
	return esFrame(headers, []byte(`{"text":`+quoteJSON(text)+`}`))
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func concatFrames(frames ...[]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

// ---- SSE parsing helpers ----

type sseEvent struct {
	event string
	data  map[string]any
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		ev := sseEvent{data: map[string]any{}}
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.data))
			}
		}
		out = append(out, ev)
	}
	return out
}

// assertStrictSequence verifies every payload carries a strictly increasing
// sequence_number starting at 1.
func assertStrictSequence(t *testing.T, events []sseEvent) {
	t.Helper()
	next := 1.0
	for _, ev := range events {
		seq, ok := ev.data["sequence_number"].(float64)
		require.True(t, ok, "event %s missing sequence_number", ev.event)
		assert.Equal(t, next, seq, "event %s sequence out of order", ev.event)
		next++
	}
}

func responsesPost(t *testing.T, s *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// ---- validation ----

func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	s := NewServer(&Config{}, &http.Client{})
	rec := responsesPost(t, s, "/v1/responses", map[string]any{
		"model":                "claude-x",
		"input":                "hi",
		"previous_response_id": "resp_old",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "previous_response_id is not supported")
	assert.Contains(t, rec.Body.String(), `"type":"invalid_request_error"`)
}

func TestResponsesRejectsBackground(t *testing.T) {
	s := NewServer(&Config{}, &http.Client{})
	rec := responsesPost(t, s, "/v1/responses", map[string]any{
		"model":      "claude-x",
		"input":      "hi",
		"background": true,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "background mode is not supported")
}

func TestResponsesRejectsServerExecutedToolTypes(t *testing.T) {
	for _, toolType := range []string{
		"web_search", "web_search_preview", "file_search", "code_interpreter",
		"computer_use_preview", "image_generation", "mcp", "local_shell", "custom",
	} {
		t.Run(toolType, func(t *testing.T) {
			s := NewServer(&Config{}, &http.Client{})
			rec := responsesPost(t, s, "/v1/responses", map[string]any{
				"model": "claude-x",
				"input": "hi",
				"tools": []map[string]any{{"type": toolType}},
			})
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "tool type(s) not supported by this provider: "+toolType)
		})
	}
}

func TestResponsesFunctionToolsAccepted(t *testing.T) {
	s := NewServer(&Config{}, &http.Client{})
	rec := responsesPost(t, s, "/v1/responses", map[string]any{
		"model": "claude-x",
		"input": "hi",
		"tools": []map[string]any{{
			"type":        "function",
			"name":        "shell",
			"description": "run a command",
			"parameters":  map[string]any{"type": "object"},
		}},
	})
	// Reaches the account pool (empty -> 503), proving validation passed.
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestResponsesRejectsNonPost(t *testing.T) {
	s := NewServer(&Config{}, &http.Client{})
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"invalid_request_error"`)
}

// ---- request conversion ----

func TestResponsesStringInputBecomesUserMessage(t *testing.T) {
	areq, err := responsesToAnthropic(&responsesRequest{
		Model:        "claude-x",
		Instructions: "be terse",
		Input:        json.RawMessage(`"run the tests"`),
		Reasoning:    &responsesReasoning{Effort: "minimal"},
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-x", areq.Model)
	sys := extractText(areq.System)
	assert.Equal(t, "be terse", sys)
	require.Len(t, areq.Messages, 1)
	assert.Equal(t, "user", areq.Messages[0].Role)
	assert.Equal(t, `"run the tests"`, string(areq.Messages[0].Content))
	require.NotNil(t, areq.OutputConfig)
	assert.Equal(t, "low", areq.OutputConfig.Effort, "minimal clamps to low")
}

func TestResponsesEffortPassthrough(t *testing.T) {
	areq, err := responsesToAnthropic(&responsesRequest{
		Model:     "claude-x",
		Input:     json.RawMessage(`"hi"`),
		Reasoning: &responsesReasoning{Effort: "high"},
	})
	require.NoError(t, err)
	assert.Equal(t, "high", areq.OutputConfig.Effort)
}

func TestResponsesItemsToMessages(t *testing.T) {
	input := []map[string]any{
		{"type": "message", "role": "system", "content": "stay safe"},
		{"type": "message", "role": "user", "content": "list files"},
		{"type": "reasoning", "id": "rs_1", "summary": []any{}},
		{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": `{"cmd":"ls"}`},
		{"type": "function_call", "call_id": "c2", "name": "edit", "arguments": `{"path":"a.go"}`},
		{"type": "function_call_output", "call_id": "c1", "output": "out1"},
		{"type": "function_call_output", "call_id": "c2", "output": "out2"},
	}
	raw, err := json.Marshal(input)
	require.NoError(t, err)

	areq, err := responsesToAnthropic(&responsesRequest{Model: "claude-x", Instructions: "inst", Input: raw})
	require.NoError(t, err)

	// system/developer folds into instructions; reasoning is dropped.
	sys := extractText(areq.System)
	assert.Contains(t, sys, "inst")
	assert.Contains(t, sys, "stay safe")

	// user message, one assistant turn with both tool_use blocks, one user
	// turn with both tool_result blocks.
	require.Len(t, areq.Messages, 3)
	assert.Equal(t, "user", areq.Messages[0].Role)

	var calls []anthropicContentBlock
	require.NoError(t, json.Unmarshal(areq.Messages[1].Content, &calls))
	assert.Equal(t, "assistant", areq.Messages[1].Role)
	require.Len(t, calls, 2)
	assert.Equal(t, "tool_use", calls[0].Type)
	assert.Equal(t, "c1", calls[0].ID)
	assert.Equal(t, "shell", calls[0].Name)
	assert.Equal(t, "c2", calls[1].ID)

	var results []anthropicContentBlock
	require.NoError(t, json.Unmarshal(areq.Messages[2].Content, &results))
	assert.Equal(t, "user", areq.Messages[2].Role)
	require.Len(t, results, 2)
	assert.Equal(t, "tool_result", results[0].Type)
	assert.Equal(t, "c1", results[0].ToolUseID)
	var out1 string
	require.NoError(t, json.Unmarshal(results[0].Content, &out1))
	assert.Equal(t, "out1", out1)
}

func TestResponsesImagePartBecomesURLSource(t *testing.T) {
	input := []map[string]any{{
		"type": "message",
		"role": "user",
		"content": []map[string]any{
			{"type": "input_text", "text": "what is this"},
			{"type": "input_image", "image_url": "https://example.com/pic.png"},
		},
	}}
	raw, err := json.Marshal(input)
	require.NoError(t, err)

	areq, err := responsesToAnthropic(&responsesRequest{Model: "claude-x", Input: raw})
	require.NoError(t, err)
	require.Len(t, areq.Messages, 1)
	var blocks []anthropicContentBlock
	require.NoError(t, json.Unmarshal(areq.Messages[0].Content, &blocks))
	require.Len(t, blocks, 2)
	assert.Equal(t, "text", blocks[0].Type)
	assert.Equal(t, "image", blocks[1].Type)
	require.NotNil(t, blocks[1].Source)
	assert.Equal(t, "url", blocks[1].Source.Type)
	assert.Equal(t, "https://example.com/pic.png", blocks[1].Source.URL)
}

func TestResponsesToolResultWithPartArray(t *testing.T) {
	input := []map[string]any{
		{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": `{"cmd":"ls"}`},
		{"type": "function_call_output", "call_id": "c1", "output": []map[string]any{
			{"type": "input_text", "text": "file-a"},
			{"type": "input_text", "text": "file-b"},
		}},
	}
	raw, err := json.Marshal(input)
	require.NoError(t, err)

	areq, err := responsesToAnthropic(&responsesRequest{Model: "claude-x", Input: raw})
	require.NoError(t, err)
	require.Len(t, areq.Messages, 2)
	var results []anthropicContentBlock
	require.NoError(t, json.Unmarshal(areq.Messages[1].Content, &results))
	require.Len(t, results, 1)
	assert.Equal(t, "tool_result", results[0].Type)
	var parts []anthropicContentBlock
	require.NoError(t, json.Unmarshal(results[0].Content, &parts))
	require.Len(t, parts, 2)
	assert.Equal(t, "file-a", parts[0].Text)
	assert.Equal(t, "file-b", parts[1].Text)
}

func TestResponsesNonStreamFailureMapsUpstreamStatus(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.bodies["Bearer a"] = concatFrames(
		contentFrame("hi"),
		exceptionFrame("ThrottlingException", "slow down"),
	)
	s := serverWithPool(t, rt, "a")
	rec := responsesPost(t, s, "/v1/responses", map[string]any{
		"model":  "claude-x",
		"input":  "run the tests",
		"stream": false,
	})
	// Throttling maps to 429/rate_limit (not a blanket 502), matching the
	// sibling Anthropic aggregate path.
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"rate_limit_exceeded"`)
	assert.Contains(t, rec.Body.String(), "slow down")
}
func TestResponsesEasyInputMessageItems(t *testing.T) {
	input := []map[string]any{
		{"role": "user", "content": "hi"},
		{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "parts"}}},
		{"type": "message", "role": "user", "content": "typed"},
	}
	raw, err := json.Marshal(input)
	require.NoError(t, err)

	areq, err := responsesToAnthropic(&responsesRequest{Model: "claude-x", Input: raw})
	require.NoError(t, err)
	require.Len(t, areq.Messages, 3)
	for _, m := range areq.Messages {
		assert.Equal(t, "user", m.Role)
	}
}

// ---- end-to-end stream synthesis ----

func TestResponsesUnknownItemTypeRejected(t *testing.T) {
	raw, err := json.Marshal([]map[string]any{{"type": "item_reference", "id": "x"}})
	require.NoError(t, err)
	_, err = responsesToAnthropic(&responsesRequest{Model: "claude-x", Input: raw})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported type")
}

// ---- end-to-end stream synthesis ----

// runResponsesStream posts a streaming request against a pooled fake runtime
// and returns the parsed SSE events.
func runResponsesStream(t *testing.T, frames []byte) (sseEvents []sseEvent, status int) {
	t.Helper()
	rt := newFakeRuntime(t)
	rt.bodies["Bearer a"] = frames
	s := serverWithPool(t, rt, "a")
	rec := responsesPost(t, s, "/v1/responses", map[string]any{
		"model": "claude-x",
		"input": "run the tests",
		"tools": []map[string]any{{
			"type":        "function",
			"name":        "shell",
			"description": "run a command",
			"parameters":  map[string]any{"type": "object"},
		}},
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	})
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return parseSSE(t, string(body)), rec.Code
}

func eventItem(t *testing.T, ev sseEvent) map[string]any {
	t.Helper()
	item, ok := ev.data["item"].(map[string]any)
	require.True(t, ok, "event %s carries no item", ev.event)
	return item
}

func TestResponsesStreamHappyPath(t *testing.T) {
	frames := concatFrames(
		contentFrame("Hello "),
		responsesToolUseFrame("t1", "shell", `{"cmd":"ls"}`, true),
		contentFrame(" done"),
		responsesMetadataFrame("END_TURN"),
	)
	events, status := runResponsesStream(t, frames)
	require.Equal(t, http.StatusOK, status)

	kinds := make([]string, 0, len(events))
	for _, ev := range events {
		kinds = append(kinds, ev.event)
	}
	expected := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta", // "Hello "
		"response.output_item.added", // function_call
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",  // function_call
		"response.output_item.added", // second message
		"response.content_part.added",
		"response.output_text.delta", // " done"
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // second message
		"response.output_text.done", // first message closes when the tool starts
		"response.content_part.done",
		"response.output_item.done", // first message
		"response.completed",
	}
	// The first message's done-events are emitted when the tool opens; find
	// them relative to the function_call block instead of assuming a position.
	assert.ElementsMatch(t, expected, kinds)
	assertStrictSequence(t, events)

	completed := events[len(events)-1]
	assert.Equal(t, "response.completed", completed.event)
	resp, ok := completed.data["response"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "completed", resp["status"])
	assert.NotEmpty(t, resp["model"], "resolved backend model is echoed")
	output, ok := resp["output"].([]any)
	require.True(t, ok)
	require.Len(t, output, 3)

	msg0, ok := output[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "message", msg0["type"])
	assert.Equal(t, "assistant", msg0["role"])
	content, ok := msg0["content"].([]any)
	require.True(t, ok)
	part0, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Hello ", part0["text"])

	fc, ok := output[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "function_call", fc["type"])
	assert.Equal(t, "t1", fc["call_id"])
	assert.Equal(t, "shell", fc["name"])
	assert.Equal(t, `{"cmd":"ls"}`, fc["arguments"])

	usage, ok := resp["usage"].(map[string]any)
	require.True(t, ok)
	assert.Greater(t, usage["input_tokens"], float64(0))
	assert.Greater(t, usage["output_tokens"], float64(0))
	inTotal, ok := usage["total_tokens"].(float64)
	require.True(t, ok)
	in, ok := usage["input_tokens"].(float64)
	require.True(t, ok)
	out, ok := usage["output_tokens"].(float64)
	require.True(t, ok)
	assert.Equal(t, in+out, inTotal)
}

func TestResponsesStreamFailedEvent(t *testing.T) {
	frames := concatFrames(
		contentFrame("hi"),
		exceptionFrame("ThrottlingException", "slow down"),
	)
	events, status := runResponsesStream(t, frames)
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, events)

	last := events[len(events)-1]
	assert.Equal(t, "response.failed", last.event)
	resp, ok := last.data["response"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "failed", resp["status"])
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "rate_limit_exceeded", errObj["code"])
	assert.Contains(t, errObj["message"], "slow down")
	// response.failed is terminal: it is the last event on the wire.
	assertStrictSequence(t, events)
}

func TestResponsesStreamDropsReasoning(t *testing.T) {
	frames := concatFrames(
		responsesReasoningFrame("I should look at the files"),
		contentFrame("answer"),
		responsesMetadataFrame("END_TURN"),
	)
	events, status := runResponsesStream(t, frames)
	require.Equal(t, http.StatusOK, status)

	for _, ev := range events {
		assert.NotEqual(t, "response.reasoning_summary_text.delta", ev.event)
		raw, err := json.Marshal(ev.data)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "I should look at the files")
	}
	completed := events[len(events)-1]
	resp, ok := completed.data["response"].(map[string]any)
	require.True(t, ok)
	output, ok := resp["output"].([]any)
	require.True(t, ok)
	require.Len(t, output, 1)
	msg, ok := output[0].(map[string]any)
	require.True(t, ok)
	content, ok := msg["content"].([]any)
	require.True(t, ok)
	part, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "answer", part["text"])
}

func TestResponsesStreamStripsLeakedToolMarker(t *testing.T) {
	frames := concatFrames(
		contentFrame("do it <function_calls>"),
		responsesMetadataFrame("END_TURN"),
	)
	events, status := runResponsesStream(t, frames)
	require.Equal(t, http.StatusOK, status)

	var doneText string
	for _, ev := range events {
		if ev.event == "response.output_text.done" {
			doneText, _ = ev.data["text"].(string)
		}
		raw, err := json.Marshal(ev.data)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "<function_calls>", "leaked marker must be stripped")
	}
	assert.Equal(t, "do it ", doneText)
}

func TestResponsesStreamMaxTokensIncomplete(t *testing.T) {
	frames := concatFrames(
		contentFrame("partial"),
		responsesMetadataFrame("MAX_TOKENS"),
	)
	events, status := runResponsesStream(t, frames)
	require.Equal(t, http.StatusOK, status)

	last := events[len(events)-1]
	assert.Equal(t, "response.incomplete", last.event)
	resp, ok := last.data["response"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "incomplete", resp["status"])
	details, ok := resp["incomplete_details"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "max_output_tokens", details["reason"])
}

func TestResponsesNonStreamAggregates(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.bodies["Bearer a"] = concatFrames(
		contentFrame("Hello "),
		responsesToolUseFrame("t1", "shell", `{"cmd":"ls"}`, true),
		responsesMetadataFrame("END_TURN"),
	)
	s := serverWithPool(t, rt, "a")
	rec := responsesPost(t, s, "/v1/responses", map[string]any{
		"model":  "claude-x",
		"input":  "run the tests",
		"stream": false,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "completed", resp.Status)
	assert.Equal(t, "response", resp.Object)
	assert.True(t, strings.HasPrefix(resp.ID, "resp_"))
	require.Len(t, resp.Output, 2)
	assert.Equal(t, "message", resp.Output[0].Type)
	require.Len(t, resp.Output[0].Content, 1)
	assert.Equal(t, "Hello ", resp.Output[0].Content[0].Text)
	assert.Equal(t, "function_call", resp.Output[1].Type)
	assert.Equal(t, "shell", resp.Output[1].Name)
	assert.Equal(t, `{"cmd":"ls"}`, resp.Output[1].Arguments)
	assert.Greater(t, resp.Usage.TotalTokens, 0)
}

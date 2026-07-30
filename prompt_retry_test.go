package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scriptedRuntimeReply struct {
	status int
	body   []byte
}

type scriptedPromptRuntime struct {
	fake *fakeRuntime

	mu       sync.Mutex
	requests []kiroRequest
	tokens   []string
}

func newScriptedPromptRuntime(t *testing.T, respond func(int, *kiroRequest, *http.Request) scriptedRuntimeReply) *scriptedPromptRuntime {
	t.Helper()
	scripted := &scriptedPromptRuntime{fake: &fakeRuntime{
		badTokens: map[string]bool{},
		bodies:    map[string][]byte{},
	}}
	scripted.fake.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve the model list without consuming a scripted call; the test's
		// respond() is only concerned with GenerateAssistantResponse sends.
		// Opus carries a schema so effort/max_tokens fields are exercised; the
		// bare sonnet matches the other models these tests request.
		if r.Header.Get("X-Amz-Target") == "KiroControlPlaneBearerService.ListAvailableModels" {
			out, _ := json.Marshal(map[string]any{"models": []kiroModelInfo{testOpusModel(), {ModelID: "claude-sonnet-4.5"}}})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var request kiroRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode Kiro request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		scripted.mu.Lock()
		scripted.requests = append(scripted.requests, request)
		scripted.tokens = append(scripted.tokens, r.Header.Get("Authorization"))
		call := len(scripted.requests)
		scripted.mu.Unlock()

		reply := respond(call, &request, r)
		w.WriteHeader(reply.status)
		if len(reply.body) > 0 {
			_, _ = w.Write(reply.body)
		}
	}))
	t.Cleanup(scripted.fake.srv.Close)
	return scripted
}

func (s *scriptedPromptRuntime) snapshot() ([]kiroRequest, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	requests := append([]kiroRequest(nil), s.requests...)
	tokens := append([]string(nil), s.tokens...)
	return requests, tokens
}

func promptTooLongReply() scriptedRuntimeReply {
	return scriptedRuntimeReply{
		status: http.StatusBadRequest,
		body:   []byte(`{"message":"prompt exceeds context","reason":"PROMPT_TOO_LONG"}`),
	}
}

func successfulEmptyStreamReply() scriptedRuntimeReply {
	return scriptedRuntimeReply{status: http.StatusOK}
}

func promptTooLongFrame() []byte {
	headers := append(
		esStringHeader(":message-type", "exception"),
		esStringHeader(":exception-type", "ValidationException")...,
	)
	return esFrame(headers, []byte(`{"message":"prompt exceeds context","reason":"PROMPT_TOO_LONG"}`))
}

func requestWithPlainTurns(turns int) *anthropicRequest {
	messages := make([]anthropicMessage, 0, 2*turns+1)
	for i := 0; i < turns; i++ {
		messages = append(messages,
			anthropicMessage{Role: "user", Content: json.RawMessage(fmt.Sprintf(`"user-%d"`, i))},
			anthropicMessage{Role: "assistant", Content: json.RawMessage(fmt.Sprintf(`"assistant-%d"`, i))},
		)
	}
	messages = append(messages, anthropicMessage{Role: "user", Content: json.RawMessage(`"current"`)})
	return &anthropicRequest{Model: "claude-sonnet-4.5", Messages: messages}
}

func buildPromptRetryRequest(t *testing.T, s *Server, areq *anthropicRequest) *kiroRequest {
	t.Helper()
	request, err := buildKiroRequest(s.cfg, areq)
	require.NoError(t, err)
	return request
}

func assertSingleAccount(t *testing.T, tokens []string) {
	t.Helper()
	require.NotEmpty(t, tokens)
	for _, token := range tokens[1:] {
		assert.Equal(t, tokens[0], token, "context retries must stay on one account")
	}
}

func historyLengths(requests []kiroRequest) []int {
	lengths := make([]int, len(requests))
	for i := range requests {
		lengths[i] = len(requests[i].ConversationState.History)
	}
	return lengths
}

func requestHasReasoning(request kiroRequest) bool {
	return hasReasoningInHistory(&request)
}

// TestOpenStreamPromptTooLongRebuildKeepsAccountModel guards the per-account
// rebuild: after a PROMPT_TOO_LONG trim, the retry must still carry the resolved
// modelId AND the clamped AdditionalModelRequestFields (not the stale global
// copy, and not dropped).
func TestOpenStreamPromptTooLongRebuildKeepsAccountModel(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(call int, _ *kiroRequest, _ *http.Request) scriptedRuntimeReply {
		if call == 1 {
			return promptTooLongReply()
		}
		return successfulEmptyStreamReply()
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := &anthropicRequest{Model: "claude-opus-4.8", MaxTokens: 4096, Messages: []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"one"`)},
		{Role: "assistant", Content: json.RawMessage(`"two"`)},
		{Role: "user", Content: json.RawMessage(`"three"`)},
		{Role: "assistant", Content: json.RawMessage(`"four"`)},
		{Role: "user", Content: json.RawMessage(`"five"`)},
	}}

	stream, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.NoError(t, err)
	require.NotNil(t, stream)
	require.NoError(t, stream.Close())

	requests, tokens := runtime.snapshot()
	require.Len(t, requests, 2, "prompt-too-long then success")
	assertSingleAccount(t, tokens)
	for i, r := range requests {
		assert.Equal(t, "claude-opus-4.8", r.ConversationState.CurrentMessage.UserInputMessage.ModelID,
			"send %d must keep the per-account resolved modelId", i)
		// JSON round-trip makes numbers float64.
		assert.Equal(t, float64(4096), r.AdditionalModelRequestFields["max_tokens"],
			"send %d must keep the clamped max_tokens after rebuild", i)
	}
}

func TestOpenStreamTrimsOneTurnUntilSuccess(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(call int, _ *kiroRequest, _ *http.Request) scriptedRuntimeReply {
		if call <= 2 {
			return promptTooLongReply()
		}
		return successfulEmptyStreamReply()
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := requestWithPlainTurns(3)

	stream, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.NoError(t, err)
	require.NotNil(t, stream)
	require.NoError(t, stream.Close())

	requests, tokens := runtime.snapshot()
	require.Len(t, requests, 3)
	assert.Equal(t, []int{6, 4, 2}, historyLengths(requests), "one oldest turn should be removed per retry")
	assertSingleAccount(t, tokens)
	require.Len(t, areq.Messages, 3)
	assert.JSONEq(t, `"user-2"`, string(areq.Messages[0].Content))
}

func TestOpenStreamStopsAfterFivePromptTooLongRetries(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(int, *kiroRequest, *http.Request) scriptedRuntimeReply {
		return promptTooLongReply()
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := requestWithPlainTurns(6)

	stream, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.Error(t, err)
	assert.Nil(t, stream)
	assert.True(t, isPromptTooLongError(err))

	requests, tokens := runtime.snapshot()
	require.Len(t, requests, 6, "initial request plus five retries")
	assert.Equal(t, []int{12, 10, 8, 6, 4, 2}, historyLengths(requests))
	assertSingleAccount(t, tokens)
	require.Len(t, areq.Messages, 3, "five oldest turns should have been removed")
}

func TestOpenStreamDoesNotRetryWithoutSafeHistory(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(int, *kiroRequest, *http.Request) scriptedRuntimeReply {
		return promptTooLongReply()
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := requestWithPlainTurns(0)

	_, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.Error(t, err)
	requests, _ := runtime.snapshot()
	assert.Len(t, requests, 1)
	assert.Len(t, areq.Messages, 1)
}

func TestOpenStreamDoesNotTrimOtherBadRequest(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(int, *kiroRequest, *http.Request) scriptedRuntimeReply {
		return scriptedRuntimeReply{
			status: http.StatusBadRequest,
			body:   []byte(`{"message":"bad input","reason":"INVALID_REQUEST"}`),
		}
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := requestWithPlainTurns(2)
	before := append([]anthropicMessage(nil), areq.Messages...)

	_, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.Error(t, err)
	requests, _ := runtime.snapshot()
	assert.Len(t, requests, 1)
	assert.Equal(t, before, areq.Messages)
}

func TestOpenStreamRetriesFirstFramePromptTooLong(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(call int, _ *kiroRequest, _ *http.Request) scriptedRuntimeReply {
		if call == 1 {
			return scriptedRuntimeReply{status: http.StatusOK, body: promptTooLongFrame()}
		}
		return successfulEmptyStreamReply()
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := requestWithPlainTurns(1)

	stream, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.NoError(t, err)
	require.NotNil(t, stream)
	require.NoError(t, stream.Close())

	requests, tokens := runtime.snapshot()
	assert.Equal(t, []int{2, 0}, historyLengths(requests))
	assertSingleAccount(t, tokens)
	assert.Len(t, areq.Messages, 1)
}

func TestOpenStreamKeepsReasoningStrippedAfterPromptTrim(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(call int, _ *kiroRequest, _ *http.Request) scriptedRuntimeReply {
		switch call {
		case 1:
			return scriptedRuntimeReply{
				status: http.StatusBadRequest,
				body:   []byte(`{"message":"bad signature","reason":"THINKING_SIGNATURE_INVALID"}`),
			}
		case 2:
			return promptTooLongReply()
		default:
			return successfulEmptyStreamReply()
		}
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := &anthropicRequest{Model: "claude-opus-4.8", Messages: []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"old"`)},
		{Role: "assistant", Content: json.RawMessage(`"old answer"`)},
		{Role: "user", Content: json.RawMessage(`"reasoned question"`)},
		{Role: "assistant", Content: json.RawMessage(`[
			{"type":"thinking","thinking":"secret","signature":"SIG=="},
			{"type":"text","text":"reasoned answer"}
		]`)},
		{Role: "user", Content: json.RawMessage(`"current"`)},
	}}

	stream, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.NoError(t, err)
	require.NotNil(t, stream)
	require.NoError(t, stream.Close())

	requests, tokens := runtime.snapshot()
	require.Len(t, requests, 3)
	assert.True(t, requestHasReasoning(requests[0]), "initial request should carry reasoning")
	assert.False(t, requestHasReasoning(requests[1]), "signature retry should strip reasoning")
	assert.False(t, requestHasReasoning(requests[2]), "trim rebuild must not reintroduce reasoning")
	assert.Equal(t, []int{4, 4, 2}, historyLengths(requests))
	assertSingleAccount(t, tokens)
}

func TestOpenStreamCancellationDuringPromptRetryDoesNotBurnAccounts(t *testing.T) {
	secondStarted := make(chan struct{}, 1)
	runtime := newScriptedPromptRuntime(t, func(call int, _ *kiroRequest, request *http.Request) scriptedRuntimeReply {
		if call == 1 {
			return promptTooLongReply()
		}
		select {
		case secondStarted <- struct{}{}:
		default:
		}
		<-request.Context().Done()
		return successfulEmptyStreamReply()
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := requestWithPlainTurns(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		stream *kiroStream
		err    error
	}
	resultCh := make(chan result, 1)
	kreq := buildPromptRetryRequest(t, s, areq)
	go func() {
		stream, err := s.openStream(ctx, kreq, areq)
		resultCh <- result{stream: stream, err: err}
	}()

	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt retry did not start")
	}
	cancel()

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled prompt retry did not return")
	}
	if got.stream != nil {
		got.stream.Close()
	}
	assert.ErrorIs(t, got.err, context.Canceled)

	requests, tokens := runtime.snapshot()
	assert.Len(t, requests, 2, "cancellation must not fail over to another account")
	assertSingleAccount(t, tokens)
	s.selector.mu.Lock()
	cooldowns := len(s.selector.cooldown)
	s.selector.mu.Unlock()
	assert.Zero(t, cooldowns, "client cancellation must not cool down healthy accounts")
}

func thinkingSignatureExceptionFrame() []byte {
	headers := append(
		esStringHeader(":message-type", "exception"),
		esStringHeader(":exception-type", "ValidationException")...,
	)
	return esFrame(headers, []byte(`{"message":"stale signature","reason":"THINKING_SIGNATURE_INVALID"}`))
}

func genericValidationExceptionFrame() []byte {
	headers := append(
		esStringHeader(":message-type", "exception"),
		esStringHeader(":exception-type", "ValidationException")...,
	)
	return esFrame(headers, []byte(`{"message":"bad request","reason":"OTHER_ERROR"}`))
}

func reasoningRequest() *anthropicRequest {
	return &anthropicRequest{Model: "claude-opus-4.8", Messages: []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"old"`)},
		{Role: "assistant", Content: json.RawMessage(`"old answer"`)},
		{Role: "user", Content: json.RawMessage(`"reasoned"`)},
		{Role: "assistant", Content: json.RawMessage(`[
			{"type":"thinking","thinking":"deep","signature":"SIG=="},
			{"type":"text","text":"answer"}
		]`)},
		{Role: "user", Content: json.RawMessage(`"current"`)},
	}}
}

func TestOpenStreamRecoversFirstFrameThinkingSignature(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(call int, _ *kiroRequest, _ *http.Request) scriptedRuntimeReply {
		if call == 1 {
			return scriptedRuntimeReply{status: http.StatusOK, body: thinkingSignatureExceptionFrame()}
		}
		return successfulEmptyStreamReply()
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := reasoningRequest()

	stream, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.NoError(t, err)
	require.NotNil(t, stream)
	require.NoError(t, stream.Close())

	requests, tokens := runtime.snapshot()
	require.Len(t, requests, 2)
	assert.True(t, requestHasReasoning(requests[0]), "first send carries reasoning")
	assert.False(t, requestHasReasoning(requests[1]), "retry must strip reasoning")
	assertSingleAccount(t, tokens)
}

func TestOpenStreamFirstFrameValidationErrorDoesNotFailover(t *testing.T) {
	runtime := newScriptedPromptRuntime(t, func(call int, _ *kiroRequest, _ *http.Request) scriptedRuntimeReply {
		return scriptedRuntimeReply{status: http.StatusOK, body: genericValidationExceptionFrame()}
	})
	s := serverWithPool(t, runtime.fake, "a", "b")
	areq := requestWithPlainTurns(2)
	before := append([]anthropicMessage(nil), areq.Messages...)

	_, err := s.openStream(context.Background(), buildPromptRetryRequest(t, s, areq), areq)
	require.Error(t, err)
	he, ok := err.(*kiroHTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadRequest, he.Status, "validation error should be 400 not 502")

	requests, tokens := runtime.snapshot()
	assert.Len(t, requests, 1, "validation error must not fail over")
	assert.Equal(t, before, areq.Messages, "must not trim history")
	assert.Len(t, tokens, 1)
	s.selector.mu.Lock()
	cooldowns := len(s.selector.cooldown)
	s.selector.mu.Unlock()
	assert.Zero(t, cooldowns, "validation error must not cool down accounts")
}

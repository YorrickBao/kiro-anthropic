package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testServerWithModels builds a Server whose model lookups resolve from a
// pre-seeded per-account cache (no network), with one account in the pool so
// anyModels can pick it.
func testServerWithModels() *Server {
	store, err := NewAccountStore(filepath.Join(os.TempDir(), "sfm-"+time.Now().Format("150405.000000")+".json"))
	if err != nil {
		panic(err)
	}
	_ = store.Add(&StoredAccount{ID: "acc", Region: "us-east-1", CreatedAt: "1"})
	s := &Server{
		cfg:         &Config{},
		selector:    newAccountSelector(store, &http.Client{}),
		accounts:    store,
		modelsCache: map[string]modelsCacheEntry{},
		usageCache:  map[string]usageCacheEntry{},
	}
	s.modelsCache["acc"] = modelsCacheEntry{
		models:  []kiroModelInfo{testOpusModel(), testSonnet45Model()},
		fetched: time.Now(),
	}
	return s
}

func reqForModel(model string) *kiroRequest {
	return &kiroRequest{ConversationState: kiroConversationState{
		CurrentMessage: kiroMessage{UserInputMessage: &kiroUserInputMessage{ModelID: model}}}}
}

func effortOf(k *kiroRequest) string {
	oc, ok := k.AdditionalModelRequestFields["output_config"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := oc["effort"].(string)
	return s
}

func TestApplyModelRequestFields(t *testing.T) {
	s := testServerWithModels()
	ctx := context.Background()

	// effort unspecified -> default max; max_tokens unspecified -> ceiling.
	k := reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 0)
	assert.Equal(t, "max", effortOf(k), "default effort")
	assert.Equal(t, 128000, k.AdditionalModelRequestFields["max_tokens"], "default max_tokens")

	// caller values honored (in range).
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "low", 5000)
	assert.Equal(t, "low", effortOf(k))
	assert.Equal(t, 5000, k.AdditionalModelRequestFields["max_tokens"])

	// max_tokens below schema minimum clamps up.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 80)
	assert.Equal(t, 1024, k.AdditionalModelRequestFields["max_tokens"], "min clamp")

	// max_tokens above ceiling clamps down.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 999999)
	assert.Equal(t, 128000, k.AdditionalModelRequestFields["max_tokens"], "max clamp")

	// unsupported effort level clamps to the model's highest.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "ultra", 0)
	assert.Equal(t, "max", effortOf(k), "ultra should clamp to max")

	// model without schema -> untouched.
	k = reqForModel("claude-sonnet-4.5")
	s.applyModelRequestFields(ctx, k, "high", 1000)
	assert.Nil(t, k.AdditionalModelRequestFields, "sonnet-4.5 should be untouched")

	// "auto" -> untouched.
	k = reqForModel("auto")
	s.applyModelRequestFields(ctx, k, "high", 1000)
	assert.Nil(t, k.AdditionalModelRequestFields, "auto should be untouched")
}

func TestModelInfoJSON(t *testing.T) {
	info := modelInfoJSON(testOpusModel(), "2026-01-01T00:00:00Z")
	assert.Equal(t, "model", info["type"])
	assert.Equal(t, "claude-opus-4.8", info["id"])
	assert.Equal(t, 1000000, info["max_input_tokens"])
	assert.Equal(t, 128000, info["max_tokens"])

	caps := info["capabilities"].(map[string]any)["effort"].(map[string]any)
	assert.Equal(t, true, caps["supported"])
	assert.Equal(t, true, caps["max"])
	assert.Equal(t, true, caps["xhigh"])

	// sonnet-4.5: no effort support.
	caps = modelInfoJSON(testSonnet45Model(), "x")["capabilities"].(map[string]any)["effort"].(map[string]any)
	assert.Equal(t, false, caps["supported"])
}

func TestMapUpstreamError(t *testing.T) {
	cases := []struct {
		status  int
		wantSt  int
		wantTyp string
	}{
		{401, 401, "authentication_error"},
		{403, 403, "permission_error"},
		{429, 429, "rate_limit_error"},
		{400, 400, "invalid_request_error"},
		{500, http.StatusBadGateway, "api_error"},
	}
	for _, c := range cases {
		st, typ := mapUpstreamError(&kiroHTTPError{Status: c.status})
		assert.Equalf(t, c.wantSt, st, "status %d", c.status)
		assert.Equalf(t, c.wantTyp, typ, "status %d", c.status)
	}
	// non-kiroHTTPError -> 502 api_error
	st, typ := mapUpstreamError(context.Canceled)
	assert.Equal(t, http.StatusBadGateway, st)
	assert.Equal(t, "api_error", typ)
}

func TestAuthorized(t *testing.T) {
	// open (no key) -> always authorized
	open := &Server{cfg: &Config{}}
	assert.True(t, open.authorized(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)),
		"open server should authorize")

	s := &Server{cfg: &Config{APIKey: "secret"}}
	mk := func(set func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		set(r)
		return r
	}
	assert.True(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("x-api-key", "secret") })),
		"matching x-api-key should authorize")
	assert.False(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("x-api-key", "wrong") })),
		"wrong x-api-key should reject")
	assert.True(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") })),
		"matching bearer should authorize")
	assert.False(t, s.authorized(mk(func(r *http.Request) {})),
		"missing key should reject")
}

func TestApplyModelRequestFieldsMinimize(t *testing.T) {
	s := testServerWithModels()
	// thinking disabled -> minimize sentinel -> the model's lowest effort level.
	k := reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(context.Background(), k, effortMinimize, 0)
	assert.Equal(t, "low", effortOf(k), "minimize effort")
}

func TestWithReasoningRetry(t *testing.T) {
	sigErr := &kiroHTTPError{Status: 400, Body: `{"reason":"THINKING_SIGNATURE_INVALID"}`}

	// history carrying reasoning on an assistant turn.
	withReasoning := func() *kiroRequest {
		return &kiroRequest{ConversationState: kiroConversationState{History: []kiroMessage{
			{UserInputMessage: &kiroUserInputMessage{Content: "hi"}},
			{AssistantResponseMessage: &kiroAssistantMessage{Content: "a",
				ReasoningContent: &kiroReasoningContent{ReasoningText: &kiroReasoningText{Text: "t", Signature: "s"}}}},
		}}}
	}
	hasReasoning := func(k *kiroRequest) bool {
		for _, m := range k.ConversationState.History {
			if m.AssistantResponseMessage != nil && m.AssistantResponseMessage.ReasoningContent != nil {
				return true
			}
		}
		return false
	}

	// 1. first send succeeds -> no retry, stream returned as-is.
	{
		want := &kiroStream{}
		calls := 0
		got, err := withReasoningRetry(withReasoning(), func(*kiroRequest) (*kiroStream, error) {
			calls++
			return want, nil
		})
		require.NoError(t, err)
		assert.Same(t, want, got)
		assert.Equal(t, 1, calls)
	}

	// 2. signature error + reasoning present -> retry once, history stripped, retry result returned.
	{
		want := &kiroStream{}
		calls := 0
		var strippedOnRetry bool
		k := withReasoning()
		got, err := withReasoningRetry(k, func(kk *kiroRequest) (*kiroStream, error) {
			calls++
			if calls == 1 {
				return nil, sigErr
			}
			strippedOnRetry = !hasReasoning(kk) // history must be stripped before the retry send
			return want, nil
		})
		require.NoError(t, err)
		assert.Same(t, want, got)
		assert.Equal(t, 2, calls)
		assert.True(t, strippedOnRetry, "history should be stripped before retry")
	}

	// 3. signature error but nothing to strip -> no retry, original error surfaces.
	{
		calls := 0
		bare := &kiroRequest{}
		_, err := withReasoningRetry(bare, func(*kiroRequest) (*kiroStream, error) {
			calls++
			return nil, sigErr
		})
		assert.Same(t, sigErr, err)
		assert.Equal(t, 1, calls)
	}

	// 4. unrelated error -> no retry, original error surfaces.
	{
		calls := 0
		other := &kiroHTTPError{Status: 400, Body: `{"reason":"PROMPT_TOO_LONG"}`}
		_, err := withReasoningRetry(withReasoning(), func(*kiroRequest) (*kiroStream, error) {
			calls++
			return nil, other
		})
		assert.Same(t, other, err)
		assert.Equal(t, 1, calls)
	}
}

func TestIsThinkingSignatureError(t *testing.T) {
	assert.True(t, isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `{"message":"bad","reason":"THINKING_SIGNATURE_INVALID"}`}),
		"reason code should match")
	// message sniff fallback when no machine reason present
	assert.True(t, isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `The thinking signature is invalid`}),
		"message sniff should match")
	assert.False(t, isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `{"reason":"PROMPT_TOO_LONG"}`}),
		"unrelated reason should not match")
	assert.False(t, isThinkingSignatureError(&kiroHTTPError{Status: 500, Body: `thinking signature`}),
		"non-400 should not match")
	assert.False(t, isThinkingSignatureError(context.Canceled),
		"non-http error should not match")
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testServerWithModels() *Server {
	return &Server{cfg: &Config{}, modelsCache: []kiroModelInfo{testOpusModel(), testSonnet45Model()}}
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
	if effortOf(k) != "max" {
		t.Errorf("default effort = %q, want max", effortOf(k))
	}
	if k.AdditionalModelRequestFields["max_tokens"] != 128000 {
		t.Errorf("default max_tokens = %v, want 128000", k.AdditionalModelRequestFields["max_tokens"])
	}

	// caller values honored (in range).
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "low", 5000)
	if effortOf(k) != "low" || k.AdditionalModelRequestFields["max_tokens"] != 5000 {
		t.Errorf("honored = %v", k.AdditionalModelRequestFields)
	}

	// max_tokens below schema minimum clamps up.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 80)
	if k.AdditionalModelRequestFields["max_tokens"] != 1024 {
		t.Errorf("min clamp = %v, want 1024", k.AdditionalModelRequestFields["max_tokens"])
	}

	// max_tokens above ceiling clamps down.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 999999)
	if k.AdditionalModelRequestFields["max_tokens"] != 128000 {
		t.Errorf("max clamp = %v, want 128000", k.AdditionalModelRequestFields["max_tokens"])
	}

	// unsupported effort level clamps to the model's highest.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "ultra", 0)
	if effortOf(k) != "max" {
		t.Errorf("ultra should clamp to max, got %q", effortOf(k))
	}

	// model without schema -> untouched.
	k = reqForModel("claude-sonnet-4.5")
	s.applyModelRequestFields(ctx, k, "high", 1000)
	if k.AdditionalModelRequestFields != nil {
		t.Errorf("sonnet-4.5 should be untouched, got %v", k.AdditionalModelRequestFields)
	}

	// "auto" -> untouched.
	k = reqForModel("auto")
	s.applyModelRequestFields(ctx, k, "high", 1000)
	if k.AdditionalModelRequestFields != nil {
		t.Errorf("auto should be untouched, got %v", k.AdditionalModelRequestFields)
	}
}

func TestModelInfoJSON(t *testing.T) {
	info := modelInfoJSON(testOpusModel(), "2026-01-01T00:00:00Z")
	if info["type"] != "model" || info["id"] != "claude-opus-4.8" {
		t.Errorf("base fields = %v", info)
	}
	if info["max_input_tokens"] != 1000000 || info["max_tokens"] != 128000 {
		t.Errorf("limits = %v", info)
	}
	caps := info["capabilities"].(map[string]any)["effort"].(map[string]any)
	if caps["supported"] != true || caps["max"] != true || caps["xhigh"] != true {
		t.Errorf("opus effort caps = %v", caps)
	}

	// sonnet-4.5: no effort support.
	caps = modelInfoJSON(testSonnet45Model(), "x")["capabilities"].(map[string]any)["effort"].(map[string]any)
	if caps["supported"] != false {
		t.Errorf("sonnet-4.5 effort caps = %v", caps)
	}
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
		if st != c.wantSt || typ != c.wantTyp {
			t.Errorf("status %d -> %d,%q; want %d,%q", c.status, st, typ, c.wantSt, c.wantTyp)
		}
	}
	// non-kiroHTTPError -> 502 api_error
	if st, typ := mapUpstreamError(context.Canceled); st != http.StatusBadGateway || typ != "api_error" {
		t.Errorf("generic error -> %d,%q", st, typ)
	}
}

func TestAuthorized(t *testing.T) {
	// open (no key) -> always authorized
	open := &Server{cfg: &Config{}}
	if !open.authorized(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)) {
		t.Errorf("open server should authorize")
	}

	s := &Server{cfg: &Config{APIKey: "secret"}}
	mk := func(set func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		set(r)
		return r
	}
	if !s.authorized(mk(func(r *http.Request) { r.Header.Set("x-api-key", "secret") })) {
		t.Errorf("matching x-api-key should authorize")
	}
	if s.authorized(mk(func(r *http.Request) { r.Header.Set("x-api-key", "wrong") })) {
		t.Errorf("wrong x-api-key should reject")
	}
	if !s.authorized(mk(func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") })) {
		t.Errorf("matching bearer should authorize")
	}
	if s.authorized(mk(func(r *http.Request) {})) {
		t.Errorf("missing key should reject")
	}
}

func TestApplyModelRequestFieldsMinimize(t *testing.T) {
	s := testServerWithModels()
	// thinking disabled -> minimize sentinel -> the model's lowest effort level.
	k := reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(context.Background(), k, effortMinimize, 0)
	if effortOf(k) != "low" {
		t.Errorf("minimize effort = %q, want low", effortOf(k))
	}
}

func TestIsThinkingSignatureError(t *testing.T) {
	if !isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `{"message":"bad","reason":"THINKING_SIGNATURE_INVALID"}`}) {
		t.Errorf("reason code should match")
	}
	// message sniff fallback when no machine reason present
	if !isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `The thinking signature is invalid`}) {
		t.Errorf("message sniff should match")
	}
	if isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `{"reason":"PROMPT_TOO_LONG"}`}) {
		t.Errorf("unrelated reason should not match")
	}
	if isThinkingSignatureError(&kiroHTTPError{Status: 500, Body: `thinking signature`}) {
		t.Errorf("non-400 should not match")
	}
	if isThinkingSignatureError(context.Canceled) {
		t.Errorf("non-http error should not match")
	}
}

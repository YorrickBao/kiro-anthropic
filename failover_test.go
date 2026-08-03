package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRuntime stands in for both runtime.<region>.kiro.dev and
// management.<region>.kiro.dev (rewriteTransport sends both to it). It returns
// 500 for accounts whose bearer token is in badTokens, and a 200 stream with
// one content frame otherwise, recording which tokens were seen.
type fakeRuntime struct {
	srv                *httptest.Server
	badTokens          map[string]bool
	unauthorizedTokens map[string]bool
	refreshAccessToken string
	refreshHook        func()
	seen               []string
	// bodies maps a bearer token to a raw 200 response body (event-stream
	// frames). When set for a token, it is served instead of the default content
	// frame, letting tests exercise post-200 in-stream outcomes.
	bodies map[string][]byte
	// modelsFor returns a per-token model list for ListAvailableModels.
	modelsFor map[string][]kiroModelInfo
	// modelsError marks tokens whose ListAvailableModels should fail, forcing the
	// mapModel fallback path.
	modelsError map[string]bool
	// invalidModel marks tokens whose GenerateAssistantResponse Send returns a
	// 400 INVALID_MODEL_ID (simulating a stale cached model list).
	invalidModel map[string]bool
	// modelsHook runs inside a model-list request before its response is written.
	// Tests use it to mutate an account while model resolution is in flight.
	modelsHook func(string)
	// Recorded on each Send: the bearer token and the modelId that was sent.
	sendTokens []string
	sentModels []string
}

func newFakeRuntime(t *testing.T, bad ...string) *fakeRuntime {
	t.Helper()
	f := &fakeRuntime{
		badTokens:          map[string]bool{},
		unauthorizedTokens: map[string]bool{},
		bodies:             map[string][]byte{},
		modelsFor:          map[string][]kiroModelInfo{},
		modelsError:        map[string]bool{},
		invalidModel:       map[string]bool{},
	}
	for _, b := range bad {
		f.badTokens[b] = true
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && f.refreshAccessToken != "" {
			if f.refreshHook != nil {
				f.refreshHook()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accessToken": f.refreshAccessToken,
				"expiresIn":   3600,
			})
			return
		}

		tok := r.Header.Get("Authorization")
		f.seen = append(f.seen, tok)

		// management endpoint: ListAvailableModels.
		if r.Header.Get("X-Amz-Target") == "KiroControlPlaneBearerService.ListAvailableModels" {
			if f.modelsError[tok] {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"models down"}`))
				return
			}
			if f.modelsHook != nil {
				f.modelsHook(tok)
			}
			w.Header().Set("Content-Type", "application/json")
			out, _ := json.Marshal(map[string]any{"models": f.modelsFor[tok]})
			_, _ = w.Write(out)
			return
		}

		// runtime endpoint: GenerateAssistantResponse — record the sent modelId.
		if raw, err := io.ReadAll(r.Body); err == nil {
			var body struct {
				ConversationState struct {
					CurrentMessage struct {
						UserInputMessage struct {
							ModelID string `json:"modelId"`
						} `json:"userInputMessage"`
					} `json:"currentMessage"`
				} `json:"conversationState"`
			}
			if json.Unmarshal(raw, &body) == nil {
				if mid := body.ConversationState.CurrentMessage.UserInputMessage.ModelID; mid != "" {
					f.sendTokens = append(f.sendTokens, tok)
					f.sentModels = append(f.sentModels, mid)
				}
			}
		}

		if f.unauthorizedTokens[tok] {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"expired"}`))
			return
		}
		if f.invalidModel[tok] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type":"com.amazon.kiro.runtimeservice#ValidationException","message":"Invalid model ID. Please select a different model to continue.","reason":"INVALID_MODEL_ID"}`))
			return
		}
		if f.badTokens[tok] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		if body, ok := f.bodies[tok]; ok {
			_, _ = w.Write(body)
		} else {
			_, _ = w.Write(contentFrame("ok"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// exceptionFrame builds a single event-stream frame carrying a service-level
// exception, mirroring how the backend surfaces an in-stream error after a 200.
func exceptionFrame(exceptionType, message string) []byte {
	headers := append(
		esStringHeader(":message-type", "exception"),
		esStringHeader(":exception-type", exceptionType)...,
	)
	payload := []byte(`{"message":"` + message + `"}`)
	return esFrame(headers, payload)
}

// contentFrame builds a single assistantResponseEvent frame with the given text.
func contentFrame(text string) []byte {
	headers := append(
		esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", "assistantResponseEvent")...,
	)
	return esFrame(headers, []byte(`{"content":"`+text+`"}`))
}

// serverWithPool builds a Server whose KiroClient is routed to the fake runtime,
// with the given accounts in the store. Accounts get a valid access token and a
// far-future expiry so accessToken() does not attempt a refresh.
func serverWithPool(t *testing.T, rt *fakeRuntime, accessTokens ...string) *Server {
	t.Helper()
	target, err := url.Parse(rt.srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for i, at := range accessTokens {
		require.NoError(t, store.Add(&StoredAccount{
			ID: at, ClientID: "c", ClientSecret: "s", RefreshToken: "r",
			Region: "us-east-1", AccessToken: at, ExpiresAt: future,
			ProfileArn: "arn:x", OverageEnabled: true, CreatedAt: string(rune('0' + i)),
		}))
	}

	cfg := &Config{}
	s := &Server{cfg: cfg, kiro: NewKiroClient(cfg, client), modelsCache: map[string]modelsCacheEntry{}}
	s.setAccounts(store, client)
	return s
}

func TestOpenStreamFailsOverToHealthyAccount(t *testing.T) {
	// "good1" is bad upstream; "good2" succeeds.
	rt := newFakeRuntime(t, "Bearer good1")
	s := serverWithPool(t, rt, "good1", "good2")

	kreq := &kiroRequest{}
	stream, err := s.openStream(context.Background(), kreq, nil)
	require.NoError(t, err, "should fail over to the healthy account")
	require.NotNil(t, stream)
	stream.Close()

	assert.Contains(t, rt.seen, "Bearer good1", "tried the failing account")
	assert.Contains(t, rt.seen, "Bearer good2", "then the healthy one")
	assert.Equal(t, "arn:x", kreq.ProfileArn)
}

func TestOpenStreamPrefersBaseBeforeOverage(t *testing.T) {
	rt := newFakeRuntime(t)
	s := serverWithPool(t, rt, "overage", "base")
	require.True(t, applyFreshUsage(t, s.selector, "overage", testOverageUsage(), usageObservationAuthoritative))
	require.True(t, applyFreshUsage(t, s.selector, "base", testBaseUsage(), usageObservationAuthoritative))

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.Equal(t, []string{"Bearer base"}, rt.seen)
}

func TestOpenStreamFallsBackToOverageAfterBaseFailure(t *testing.T) {
	rt := newFakeRuntime(t, "Bearer base")
	s := serverWithPool(t, rt, "overage", "base")
	require.True(t, applyFreshUsage(t, s.selector, "overage", testOverageUsage(), usageObservationAuthoritative))
	require.True(t, applyFreshUsage(t, s.selector, "base", testBaseUsage(), usageObservationAuthoritative))

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.Equal(t, []string{"Bearer base", "Bearer overage"}, rt.seen)
}

func TestMessagesUsesClaudeCodeSessionAffinity(t *testing.T) {
	const body = `{"model":"gpt-4o","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	send := func(t *testing.T, handler http.Handler, sessionID string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		if sessionID != "" {
			req.Header.Set(claudeCodeSessionIDHeader, sessionID)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	t.Run("same session", func(t *testing.T) {
		rt := newFakeRuntime(t)
		for _, id := range []string{"a", "b", "c"} {
			rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
		}
		s := serverWithPool(t, rt, "a", "b", "c")
		handler := s.Handler()
		const sessionID = "550e8400-e29b-41d4-a716-446655440000"
		expected := requireLease(t, s.selector.pickFor(map[string]bool{}, sessionID)).creds.id

		for i := 0; i < 3; i++ {
			send(t, handler, sessionID)
		}
		assert.Equal(t, []string{"Bearer " + expected, "Bearer " + expected, "Bearer " + expected}, rt.sendTokens)
	})

	t.Run("maximum length session", func(t *testing.T) {
		rt := newFakeRuntime(t)
		for _, id := range []string{"a", "b"} {
			rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
		}
		s := serverWithPool(t, rt, "a", "b")
		handler := s.Handler()
		sessionID := strings.Repeat("s", maxClaudeCodeSessionIDBytes)
		expected := requireLease(t, s.selector.pickFor(map[string]bool{}, sessionID)).creds.id

		send(t, handler, sessionID)
		send(t, handler, sessionID)

		assert.Equal(t, []string{"Bearer " + expected, "Bearer " + expected}, rt.sendTokens)
	})

	t.Run("oversized session falls back to round robin", func(t *testing.T) {
		rt := newFakeRuntime(t)
		for _, id := range []string{"a", "b"} {
			rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
		}
		s := serverWithPool(t, rt, "a", "b")
		handler := s.Handler()
		sessionID := strings.Repeat("s", maxClaudeCodeSessionIDBytes+1)

		send(t, handler, sessionID)
		send(t, handler, sessionID)

		assert.Equal(t, []string{"Bearer a", "Bearer b"}, rt.sendTokens)
	})

	t.Run("missing or blank session", func(t *testing.T) {
		rt := newFakeRuntime(t)
		for _, id := range []string{"a", "b"} {
			rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
		}
		s := serverWithPool(t, rt, "a", "b")
		handler := s.Handler()

		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set(claudeCodeSessionIDHeader, "   ")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		send(t, handler, "")

		assert.Equal(t, []string{"Bearer a", "Bearer b"}, rt.sendTokens)
	})
}

func TestOpenStreamSessionAffinityFailsOverDeterministically(t *testing.T) {
	rt := newFakeRuntime(t)
	s := serverWithPool(t, rt, "a", "b", "c")
	const sessionID = "runtime-failover-session"
	primary := requireLease(t, s.selector.pickFor(map[string]bool{}, sessionID)).creds.id
	secondary := requireLease(t, s.selector.pickFor(map[string]bool{primary: true}, sessionID)).creds.id
	rt.badTokens["Bearer "+primary] = true
	ctx := context.WithValue(context.Background(), ctxKeyAccountAffinity{}, sessionID)

	stream, err := s.openStream(ctx, &kiroRequest{}, nil)
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.Equal(t, []string{"Bearer " + primary, "Bearer " + secondary}, rt.seen)
}

func TestOpenStreamSessionAffinityModelSkipsDoNotConsumeSendBudget(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	const sessionID = "model-skip-session"
	serve := accounts[0]
	for _, id := range accounts[1:] {
		score := accountAffinityScore(sessionID, id)
		serveScore := accountAffinityScore(sessionID, serve)
		if score < serveScore || (score == serveScore && id > serve) {
			serve = id
		}
	}

	rt := newFakeRuntime(t)
	for _, id := range accounts {
		modelID := "claude-opus-4.8"
		if id == serve {
			modelID = "gpt-5.6-sol"
		}
		rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: modelID}}
	}
	s := serverWithPool(t, rt, accounts...)
	ctx := context.WithValue(context.Background(), ctxKeyAccountAffinity{}, sessionID)

	stream, err := s.openStream(ctx, &kiroRequest{}, msgAreq("gpt-4o"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.Equal(t, []string{"Bearer " + serve}, rt.sendTokens)
}

func TestOpenStreamAllAccountsFail(t *testing.T) {
	rt := newFakeRuntime(t, "Bearer a", "Bearer b")
	s := serverWithPool(t, rt, "a", "b")

	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.Error(t, err, "no healthy account -> error surfaces")
	he, ok := err.(*kiroHTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusInternalServerError, he.Status)
}

// TestOpenStreamFailsOverOnFirstFrameException verifies that an upstream error
// arriving as the very first stream frame (a 200 followed by an exception, with
// no content produced) triggers pre-stream failover to a healthy account rather
// than surfacing mid-stream.
func TestOpenStreamFailsOverOnFirstFrameException(t *testing.T) {
	rt := newFakeRuntime(t)
	// "bad" returns 200 then immediately errors; "good" returns 200 + content.
	rt.bodies["Bearer bad"] = exceptionFrame("InternalServerException",
		"Encountered an unexpected error when processing the request, please try again.")
	rt.bodies["Bearer good"] = contentFrame("hi")
	s := serverWithPool(t, rt, "bad", "good")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err, "first-frame exception should fail over to healthy account")
	require.NotNil(t, stream)
	defer stream.Close()

	// The healthy stream's first event must be replayed intact by Recv.
	ev, err := stream.Recv()
	require.NoError(t, err)
	assert.Equal(t, evText, ev.Kind)
	assert.Equal(t, "hi", ev.Text)

	assert.Contains(t, rt.seen, "Bearer bad", "tried the failing account")
	assert.Contains(t, rt.seen, "Bearer good", "then the healthy one")
}

// TestOpenStreamAllFirstFrameExceptions verifies that when every account errors
// on the first frame, the upstream error surfaces instead of a healthy stream.
func TestOpenStreamAllFirstFrameExceptions(t *testing.T) {
	rt := newFakeRuntime(t)
	body := exceptionFrame("InternalServerException", "please try again")
	rt.bodies["Bearer a"] = body
	rt.bodies["Bearer b"] = body
	s := serverWithPool(t, rt, "a", "b")

	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.Error(t, err)
	he, ok := err.(*kiroHTTPError)
	require.True(t, ok, "want kiroHTTPError, got %T", err)
	assert.Equal(t, http.StatusBadGateway, he.Status)
}

func TestOpenStreamAllFirstFrameThrottledReturns429(t *testing.T) {
	rt := newFakeRuntime(t)
	body := errorEventFrame("throttlingError", "slow down")
	rt.bodies["Bearer a"] = body
	rt.bodies["Bearer b"] = body
	s := serverWithPool(t, rt, "a", "b")

	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.Error(t, err)
	he, ok := err.(*kiroHTTPError)
	require.True(t, ok, "want kiroHTTPError, got %T", err)
	assert.Equal(t, http.StatusTooManyRequests, he.Status)
	assert.Equal(t, []string{"Bearer a", "Bearer b"}, rt.seen)
}

// TestDispatchFairnessTwoAccounts verifies that with 2 healthy accounts,
// consecutive dispatches alternate evenly (round-robin actually rotates).
func TestDispatchFairnessTwoAccounts(t *testing.T) {
	rt := newFakeRuntime(t) // both accounts healthy
	s := serverWithPool(t, rt, "a", "b")

	for i := 0; i < 6; i++ {
		stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
		require.NoError(t, err)
		stream.Close()
	}
	served := map[string]int{}
	for _, tok := range rt.seen {
		served[tok]++
	}
	assert.Equal(t, 3, served["Bearer a"], "got %v", served)
	assert.Equal(t, 3, served["Bearer b"], "got %v", served)
}

func TestOpenStreamEmptyPool(t *testing.T) {
	rt := newFakeRuntime(t)
	s := serverWithPool(t, rt) // no accounts
	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.ErrorIs(t, err, errNoAccount)
}

func TestOpenStreamRequestErrorDoesNotBurnAccounts(t *testing.T) {
	// 400 (not a thinking-signature error) is a request problem: stop immediately.
	rt := &fakeRuntime{badTokens: map[string]bool{}}
	rt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt.seen = append(rt.seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad input"}`))
	}))
	t.Cleanup(rt.srv.Close)
	s := serverWithPool(t, rt, "a", "b", "c")

	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.Error(t, err)
	assert.Len(t, rt.seen, 1, "a 400 should not try further accounts")
}

// errorEventFrame builds a normal event-stream error union member.
func errorEventFrame(kind, message string) []byte {
	headers := append(
		esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", kind)...,
	)
	return esFrame(headers, []byte(`{"message":"`+message+`"}`))
}

// quotaErrorFrame builds a single serviceQuotaExceededError event frame.
func quotaErrorFrame(message string) []byte {
	return errorEventFrame("serviceQuotaExceededError", message)
}

// TestOpenStreamQuotaErrorParksDepleted verifies that a serviceQuotaExceededError
// arriving as the first frame fails over to a healthy account AND parks the
// exhausted one as depleted (not the short cooldown), so it is not retried
// every 60s.
func TestOpenStreamQuotaErrorParksDepleted(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.bodies["Bearer bad"] = quotaErrorFrame("You have exceeded your service quota")
	rt.bodies["Bearer good"] = contentFrame("hi")
	s := serverWithPool(t, rt, "bad", "good")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err, "quota error should fail over to the healthy account")
	require.NotNil(t, stream)
	defer stream.Close()

	// The exhausted account is parked as depleted, distinct from the cooldown.
	s.selector.mu.Lock()
	state := s.selector.states["bad"]
	depleted := state != nil && state.quota == quotaDepleted && state.reactive
	cooling := state != nil && time.Now().Before(state.cooldownUntil)
	s.selector.mu.Unlock()
	assert.True(t, depleted, "quota-exhausted account is parked as depleted")
	assert.False(t, cooling, "depleted is distinct from the short cooldown")
}

func TestOpenStreamLateQuotaErrorParksDepleted(t *testing.T) {
	rt := newFakeRuntime(t)
	body := append(contentFrame("hi"), quotaErrorFrame("late quota")...)
	rt.bodies["Bearer acc"] = body
	s := serverWithPool(t, rt, "acc")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()

	// firstFrameFailure primed this content event; Recv must replay and observe
	// it once without changing quota state.
	ev, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, evText, ev.Kind)
	assert.False(t, s.selector.isReactivelyDepleted("acc", runtimeRevision(t, s.accounts, "acc")))

	ev, err = stream.Recv()
	require.NoError(t, err)
	require.Equal(t, evError, ev.Kind)
	assert.Equal(t, "serviceQuotaExceededError", ev.ErrKind)
	assert.True(t, s.selector.isReactivelyDepleted("acc", runtimeRevision(t, s.accounts, "acc")))
}

func TestMessagesLateEventPreservesErrorMapping(t *testing.T) {
	requestBody := `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name    string
		frame   []byte
		status  int
		errType string
	}{
		{"quota", quotaErrorFrame("credit exhausted"), http.StatusPaymentRequired, "api_error"},
		{"validation", exceptionFrame("ValidationException", "bad request"), http.StatusBadRequest, "invalid_request_error"},
		{"throttle", errorEventFrame("throttlingError", "slow down"), http.StatusTooManyRequests, "rate_limit_error"},
		{"upstream", exceptionFrame("InternalServerException", "failed"), http.StatusBadGateway, "api_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime(t)
			rt.bodies["Bearer acc"] = append(contentFrame("partial"), tc.frame...)
			s := serverWithPool(t, rt, "acc")
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
			rec := httptest.NewRecorder()

			s.Handler().ServeHTTP(rec, req)

			assert.Equal(t, tc.status, rec.Code)
			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.errType, body.Error.Type)
		})
	}
}

func TestStreamingLateEventPreservesErrorType(t *testing.T) {
	requestBody := `{"model":"claude-sonnet-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name    string
		frame   []byte
		errType string
	}{
		{"validation", exceptionFrame("ValidationException", "bad request"), "invalid_request_error"},
		{"throttle", errorEventFrame("throttlingError", "slow down"), "rate_limit_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime(t)
			rt.bodies["Bearer acc"] = append(contentFrame("partial"), tc.frame...)
			s := serverWithPool(t, rt, "acc")
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
			rec := httptest.NewRecorder()

			s.Handler().ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), `"type":"`+tc.errType+`"`)
		})
	}
}

func TestOpenStreamLateQuotaObservedExactlyOnce(t *testing.T) {
	rt := newFakeRuntime(t)
	body := append(contentFrame("hi"), quotaErrorFrame("first")...)
	body = append(body, quotaErrorFrame("duplicate")...)
	rt.bodies["Bearer acc"] = body
	s := serverWithPool(t, rt, "acc")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()
	_, err = stream.Recv() // replay primed content
	require.NoError(t, err)

	s.selector.mu.Lock()
	before := s.selector.states["acc"].generation
	s.selector.mu.Unlock()
	for range 2 {
		ev, recvErr := stream.Recv()
		require.NoError(t, recvErr)
		require.Equal(t, evError, ev.Kind)
	}
	s.selector.mu.Lock()
	state := s.selector.states["acc"]
	after := state.generation
	reactive := state.reactive
	s.selector.mu.Unlock()
	assert.True(t, reactive)
	assert.Equal(t, before+1, after, "duplicate late quota events must not mutate selector twice")
}

func TestOpenStreamLateAccountErrorStartsCooldown(t *testing.T) {
	rt := newFakeRuntime(t)
	body := append(contentFrame("hi"), exceptionFrame("InternalServerException", "late failure")...)
	rt.bodies["Bearer acc"] = body
	s := serverWithPool(t, rt, "acc")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()
	_, err = stream.Recv()
	require.NoError(t, err)
	ev, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, evError, ev.Kind)

	s.selector.mu.Lock()
	cooling := time.Now().Before(s.selector.states["acc"].cooldownUntil)
	s.selector.mu.Unlock()
	assert.True(t, cooling, "late account failure must cool the account for future routing")
}

func TestOpenStreamLateTransportErrorStartsCooldown(t *testing.T) {
	rt := newFakeRuntime(t)
	body := append(contentFrame("hi"), []byte{0, 0, 0, 20, 0, 0}...)
	rt.bodies["Bearer acc"] = body
	s := serverWithPool(t, rt, "acc")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()
	_, err = stream.Recv()
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err)

	s.selector.mu.Lock()
	cooling := time.Now().Before(s.selector.states["acc"].cooldownUntil)
	s.selector.mu.Unlock()
	assert.True(t, cooling, "late decoder/transport failure must cool the account")
}

func TestOpenStreamLateQuotaUpgradesPriorCooldown(t *testing.T) {
	rt := newFakeRuntime(t)
	body := append(contentFrame("hi"), exceptionFrame("InternalServerException", "late failure")...)
	body = append(body, quotaErrorFrame("quota after failure")...)
	rt.bodies["Bearer acc"] = body
	s := serverWithPool(t, rt, "acc")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()
	for range 3 {
		_, err = stream.Recv()
		require.NoError(t, err)
	}
	assert.True(t, s.selector.isReactivelyDepleted("acc", runtimeRevision(t, s.accounts, "acc")))
}

// TestDepletedAccountSkippedOnNextRequest verifies the preventive effect: once
// parked, an exhausted account is not retried on the next request (it would be
// re-tried every time under the old reactive-only cooldown behavior).
func TestDepletedAccountSkippedOnNextRequest(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.bodies["Bearer bad"] = quotaErrorFrame("exceeded")
	rt.bodies["Bearer good"] = contentFrame("hi")
	s := serverWithPool(t, rt, "bad", "good")

	// First request: tries "bad" (quota error -> depleted), then "good".
	st1, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	st1.Close()

	rt.seen = nil
	// Second request: "bad" is depleted -> skip straight to "good".
	st2, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	st2.Close()

	assert.NotContains(t, rt.seen, "Bearer bad", "depleted account is not retried on the next request")
	assert.Contains(t, rt.seen, "Bearer good")
}

func TestMessagesKnownDepletedPoolReturns402WithoutRuntimeProbe(t *testing.T) {
	var usageCalls int
	var runtimeCalls int
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/getUsageLimits" {
			usageCalls++
			writeUsageResponse(w, 0)
			return
		}
		runtimeCalls++
		http.Error(w, "unexpected runtime request", http.StatusInternalServerError)
	})
	body := `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusPaymentRequired, rec.Code)
		assert.Contains(t, rec.Body.String(), "account pool quota exhausted")
	}
	assert.Equal(t, 1, usageCalls, "sticky depletion should avoid repeated request-path usage checks")
	assert.Zero(t, runtimeCalls, "known depletion must never be verified with inference traffic")
}

// msgAreq builds a minimal non-nil Anthropic request so openStream takes the
// per-account model-resolution path (areq != nil).
func msgAreq(model string) *anthropicRequest {
	return &anthropicRequest{
		Model:     model,
		MaxTokens: 16,
		Messages:  []anthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
}

// TestOpenStreamRoutesToAccountServingModel: a "gpt-4o" request must skip the
// Claude-only account and land on the GPT account, sending its concrete modelId.
func TestOpenStreamRoutesToAccountServingModel(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.modelsFor["Bearer claude"] = []kiroModelInfo{{ModelID: "claude-opus-4.8"}}
	rt.modelsFor["Bearer gpt"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	s := serverWithPool(t, rt, "claude", "gpt")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-4o"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()

	require.Len(t, rt.sentModels, 1, "exactly one Send should occur")
	assert.Equal(t, "gpt-5.6-sol", rt.sentModels[0], "resolved to the account's concrete model")
	assert.Equal(t, []string{"Bearer gpt"}, rt.sendTokens, "routed to the gpt account only")
}

// TestOpenStreamSkipsAllAccountsMissingModel: when no account serves the model,
// nothing is sent and errModelUnavailable surfaces (mapped to 400 by the handler).
func TestOpenStreamSkipsAllAccountsMissingModel(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.modelsFor["Bearer a"] = []kiroModelInfo{{ModelID: "claude-opus-4.8"}}
	rt.modelsFor["Bearer b"] = []kiroModelInfo{{ModelID: "claude-sonnet-4.5"}}
	s := serverWithPool(t, rt, "a", "b")

	_, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-4o"))
	require.ErrorIs(t, err, errModelUnavailable)
	assert.Empty(t, rt.sentModels, "no Send should occur when no account serves the model")
}

func TestOpenStreamModelSkipsDoNotConsumeRuntimeAttemptBudget(t *testing.T) {
	rt := newFakeRuntime(t)
	accounts := []string{"skip0", "skip1", "skip2", "skip3", "skip4", "skip5", "skip6", "skip7", "skip8", "skip9", "serve"}
	for _, id := range accounts[:10] {
		rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "claude-opus-4.8"}}
	}
	rt.modelsFor["Bearer serve"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	rt.bodies["Bearer serve"] = contentFrame("hi")
	s := serverWithPool(t, rt, accounts...)

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-4o"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.Equal(t, []string{"Bearer serve"}, rt.sendTokens)
}

func TestOpenStreamCapsActualRuntimeSends(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	bad := make([]string, len(accounts))
	for i, id := range accounts {
		bad[i] = "Bearer " + id
	}
	rt := newFakeRuntime(t, bad...)
	s := serverWithPool(t, rt, accounts...)

	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.Error(t, err)
	assert.Len(t, rt.seen, maxAccountAttempts)
	assert.Equal(t, bad[:maxAccountAttempts], rt.seen)
}

func TestOpenStreamCapsPhysicalRuntimeSendsAcrossAuthRetries(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e"}
	rt := newFakeRuntime(t)
	rt.refreshAccessToken = "refreshed"
	rt.badTokens["Bearer refreshed"] = true
	for _, id := range accounts {
		rt.unauthorizedTokens["Bearer "+id] = true
	}
	s := serverWithPool(t, rt, accounts...)

	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.Error(t, err)
	expected := []string{
		"Bearer a", "Bearer refreshed",
		"Bearer b", "Bearer refreshed",
		"Bearer c", "Bearer refreshed",
		"Bearer d", "Bearer refreshed",
	}
	assert.Equal(t, expected, rt.seen, "post-refresh sends must consume the same physical-send budget")
}

func TestOpenStreamRevalidatesLeaseBeforeAuthRetry(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.unauthorizedTokens["Bearer acc"] = true
	rt.refreshAccessToken = "refreshed"
	s := serverWithPool(t, rt, "acc")
	mutated := make(chan error, 1)
	rt.refreshHook = func() {
		mutated <- s.accounts.SetDisabled("acc", true)
	}

	_, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.Error(t, err)
	require.NoError(t, <-mutated)
	var he *kiroHTTPError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusUnauthorized, he.Status, "the last physical response should be preserved")
	assert.Equal(t, []string{"Bearer acc"}, rt.seen, "disabled lease must not issue the post-refresh request")
}

func TestOpenStreamRepicksRevisionChangedBeforeFirstSend(t *testing.T) {
	for _, profileKnown := range []bool{true, false} {
		name := "send token refresh"
		if !profileKnown {
			name = "profile token refresh"
		}
		t.Run(name, func(t *testing.T) {
			rt := newFakeRuntime(t)
			rt.refreshAccessToken = "stale-refresh-result"
			rt.bodies["Bearer replacement"] = contentFrame("hi")
			s := serverWithPool(t, rt, "acc")

			stale, ok := s.accounts.Get("acc")
			require.True(t, ok)
			stale.AccessToken = ""
			stale.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			if !profileKnown {
				stale.ProfileArn = ""
			}
			require.NoError(t, s.accounts.ReplaceCredentials("acc", &stale))

			replaced := make(chan error, 1)
			rt.refreshHook = func() {
				fresh, ok := s.accounts.Get("acc")
				if !ok {
					replaced <- errNoAccount
					return
				}
				fresh.AccessToken = "replacement"
				fresh.ExpiresAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
				fresh.ProfileArn = "arn:replacement"
				replaced <- s.accounts.ReplaceCredentials("acc", &fresh)
			}

			stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
			require.NoError(t, err)
			require.NotNil(t, stream)
			stream.Close()
			require.NoError(t, <-replaced)
			assert.Equal(t, []string{"Bearer replacement"}, rt.seen,
				"stale refresh must not consume the replacement account or issue runtime traffic")
		})
	}
}

func TestOpenStreamRevalidatesLeaseAfterModelLookup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *Server)
	}{
		{name: "disable", mutate: func(t *testing.T, s *Server) {
			require.NoError(t, s.accounts.SetDisabled("acc", true))
		}},
		{name: "remove", mutate: func(t *testing.T, s *Server) {
			require.NoError(t, s.accounts.Remove("acc"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeRuntime(t)
			rt.modelsFor["Bearer acc"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
			started := make(chan struct{})
			release := make(chan struct{})
			rt.modelsHook = func(string) {
				close(started)
				<-release
			}
			s := serverWithPool(t, rt, "acc")
			errCh := make(chan error, 1)
			go func() {
				_, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-4o"))
				errCh <- err
			}()
			<-started
			tc.mutate(t, s)
			close(release)
			require.Error(t, <-errCh)
			assert.Empty(t, rt.sendTokens, "stale lease must not reach runtime")
		})
	}
}

func TestOpenStreamRepicksWhenBaseEntersOverageBeforeSend(t *testing.T) {
	rt := newFakeRuntime(t)
	for _, id := range []string{"base", "overage"} {
		rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	}
	s := serverWithPool(t, rt, "overage", "base")
	require.True(t, applyFreshUsage(t, s.selector, "overage", testOverageUsage(), usageObservationAuthoritative))
	require.True(t, applyFreshUsage(t, s.selector, "base", testBaseUsage(), usageObservationAuthoritative))

	mutated := make(chan bool, 1)
	var once sync.Once
	rt.modelsHook = func(token string) {
		if token != "Bearer base" {
			return
		}
		once.Do(func() {
			_, stamp, ok := s.selector.usageTarget("base")
			mutated <- ok && s.selector.applyUsage(stamp, testOverageUsage(), usageObservationAuthoritative)
		})
	}

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-5.6-sol"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.True(t, <-mutated)
	assert.Equal(t, []string{"Bearer overage"}, rt.sendTokens,
		"the stale base lease must be repicked before runtime traffic")
}

func TestOpenStreamRepicksWhenDifferentAccountBecomesBaseBeforeSend(t *testing.T) {
	rt := newFakeRuntime(t)
	for _, id := range []string{"selected", "recovered"} {
		rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	}
	s := serverWithPool(t, rt, "selected", "recovered")
	for _, id := range []string{"selected", "recovered"} {
		require.True(t, applyFreshUsage(t, s.selector, id, testOverageUsage(), usageObservationAuthoritative))
	}

	mutated := make(chan bool, 1)
	var once sync.Once
	rt.modelsHook = func(token string) {
		if token != "Bearer selected" {
			return
		}
		once.Do(func() {
			_, stamp, ok := s.selector.usageTarget("recovered")
			mutated <- ok && s.selector.applyUsage(stamp, testBaseUsage(), usageObservationAuthoritative)
		})
	}

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-5.6-sol"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.True(t, <-mutated)
	assert.Equal(t, []string{"Bearer recovered"}, rt.sendTokens,
		"a ready Base account must supersede the previously selected Overage lease")
}

func TestOpenStreamRequeuedOverageRemainsFallbackAfterPreferredModelSkip(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.modelsFor["Bearer selected"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	rt.modelsFor["Bearer preferred"] = []kiroModelInfo{{ModelID: "claude-opus-4.8"}}
	s := serverWithPool(t, rt, "selected", "preferred")
	for _, id := range []string{"selected", "preferred"} {
		require.True(t, applyFreshUsage(t, s.selector, id, testOverageUsage(), usageObservationAuthoritative))
	}

	mutated := make(chan bool, 1)
	var once sync.Once
	rt.modelsHook = func(token string) {
		if token != "Bearer selected" {
			return
		}
		once.Do(func() {
			_, stamp, ok := s.selector.usageTarget("preferred")
			mutated <- ok && s.selector.applyUsage(stamp, testBaseUsage(), usageObservationAuthoritative)
		})
	}

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-5.6-sol"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.True(t, <-mutated)
	assert.Equal(t, []string{"Bearer selected"}, rt.sendTokens,
		"the unsent Overage lease must remain available after the preferred account is model-skipped")
}

func TestOpenStreamSoftSupersessionCapAvoidsLivelock(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.modelsFor["Bearer selected"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	s := serverWithPool(t, rt, "selected")
	require.True(t, applyFreshUsage(t, s.selector, "selected", testOverageUsage(), usageObservationAuthoritative))

	candidate := 0
	rt.modelsHook = func(token string) {
		if token == "Bearer selected" {
			id := fmt.Sprintf("preferred-%d", candidate)
			candidate++
			rt.modelsFor["Bearer "+id] = []kiroModelInfo{{ModelID: "claude-opus-4.8"}}
			_ = s.accounts.Add(&StoredAccount{
				ID: id, AccessToken: id, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				ProfileArn: "arn:" + id, OverageEnabled: true, CreatedAt: fmt.Sprintf("%03d", candidate),
			})
			return
		}
		if strings.HasPrefix(token, "Bearer preferred-") {
			s.invalidateModels("selected")
		}
	}

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-5.6-sol"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.Equal(t, maxPreSendLeaseRefreshes+1, candidate)
	assert.Equal(t, []string{"Bearer selected"}, rt.sendTokens,
		"a hard-valid Overage lease may send once the soft-supersession churn cap is reached")
}

func TestOpenStreamRepicksFallbackLeaseAfterCooldownExpires(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.modelsFor["Bearer acc"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	s := serverWithPool(t, rt, "acc")

	// Put the sole account in cooldown so pick returns it as the availability
	// fallback, then expire that cooldown while model preparation is in flight.
	initial := requireLease(t, s.selector.pick(map[string]bool{}))
	s.selector.recordFailure(initial)
	rt.modelsHook = func(string) {
		s.selector.mu.Lock()
		s.selector.states["acc"].cooldownUntil = time.Now().Add(-time.Second)
		s.selector.mu.Unlock()
	}

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-5.6-sol"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()
	assert.Equal(t, []string{"Bearer acc"}, rt.sendTokens)
}

// TestOpenStreamInvalidModelFailsOver: a stale cached list that causes an
// INVALID_MODEL_ID is invalidated and the request fails over to a good account.
func TestOpenStreamInvalidModelFailsOver(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.modelsFor["Bearer stale"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	rt.modelsFor["Bearer good"] = []kiroModelInfo{{ModelID: "gpt-5.6-sol"}}
	rt.invalidModel["Bearer stale"] = true
	rt.bodies["Bearer good"] = contentFrame("hi")
	s := serverWithPool(t, rt, "stale", "good")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("gpt-5.6-sol"))
	require.NoError(t, err, "INVALID_MODEL_ID should fail over to the good account")
	require.NotNil(t, stream)
	stream.Close()

	assert.Contains(t, rt.sendTokens, "Bearer stale")
	assert.Contains(t, rt.sendTokens, "Bearer good")
	s.modelsMu.Lock()
	_, present := s.modelsCache["stale"]
	s.modelsMu.Unlock()
	assert.False(t, present, "stale model cache should be invalidated after INVALID_MODEL_ID")
}

// TestOpenStreamColdCacheFallsBackToMapModel: when an account's model list can't
// be fetched, the static mapModel guess is sent so a transient failure can't
// regress behavior.
func TestOpenStreamColdCacheFallsBackToMapModel(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.modelsError["Bearer a"] = true
	s := serverWithPool(t, rt, "a")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("claude-opus-4-8-20260101"))
	require.NoError(t, err)
	require.NotNil(t, stream)
	stream.Close()

	require.Len(t, rt.sentModels, 1)
	assert.Equal(t, "claude-opus-4.8", rt.sentModels[0], "dated name maps to claude-opus-4.8 via the static fallback")
}

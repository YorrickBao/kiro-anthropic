package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRuntime stands in for runtime.<region>.kiro.dev. It returns 500 for
// accounts whose bearer token is in badTokens, and 200 (empty stream) otherwise,
// recording which tokens were seen.
type fakeRuntime struct {
	srv       *httptest.Server
	badTokens map[string]bool
	seen      []string
	// bodies maps a bearer token to a raw 200 response body (event-stream
	// frames). When set for a token, it is served instead of an empty stream,
	// letting tests exercise post-200 in-stream outcomes.
	bodies map[string][]byte
}

func newFakeRuntime(t *testing.T, bad ...string) *fakeRuntime {
	t.Helper()
	f := &fakeRuntime{badTokens: map[string]bool{}, bodies: map[string][]byte{}}
	for _, b := range bad {
		f.badTokens[b] = true
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get("Authorization")
		f.seen = append(f.seen, tok)
		if f.badTokens[tok] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusOK) // 200: a stream (empty unless a body is set)
		if body, ok := f.bodies[tok]; ok {
			_, _ = w.Write(body)
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
			ProfileArn: "arn:x", CreatedAt: string(rune('0' + i)),
		}))
	}

	cfg := &Config{}
	s := &Server{cfg: cfg, kiro: NewKiroClient(cfg, client)}
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

// quotaErrorFrame builds a single event-stream frame carrying a
// serviceQuotaExceededError as a normal event (:message-type=event), mirroring
// how the backend surfaces an exhausted-credit error after a 200.
func quotaErrorFrame(message string) []byte {
	headers := append(
		esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", "serviceQuotaExceededError")...,
	)
	return esFrame(headers, []byte(`{"message":"`+message+`"}`))
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
	_, depleted := s.selector.depleted["bad"]
	_, cooling := s.selector.cooldown["bad"]
	s.selector.mu.Unlock()
	assert.True(t, depleted, "quota-exhausted account is parked as depleted")
	assert.False(t, cooling, "depleted is distinct from the short cooldown")
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

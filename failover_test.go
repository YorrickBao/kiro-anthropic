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
}

func newFakeRuntime(t *testing.T, bad ...string) *fakeRuntime {
	t.Helper()
	f := &fakeRuntime{badTokens: map[string]bool{}}
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
		w.WriteHeader(http.StatusOK) // empty event stream; Send returns a stream
	}))
	t.Cleanup(f.srv.Close)
	return f
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
	stream, err := s.openStream(context.Background(), kreq)
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

	_, err := s.openStream(context.Background(), &kiroRequest{})
	require.Error(t, err, "no healthy account -> error surfaces")
	he, ok := err.(*kiroHTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusInternalServerError, he.Status)
}

// TestDispatchFairnessTwoAccounts verifies that with 2 healthy accounts,
// consecutive dispatches alternate evenly (round-robin actually rotates).
func TestDispatchFairnessTwoAccounts(t *testing.T) {
	rt := newFakeRuntime(t) // both accounts healthy
	s := serverWithPool(t, rt, "a", "b")

	for i := 0; i < 6; i++ {
		stream, err := s.openStream(context.Background(), &kiroRequest{})
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
	_, err := s.openStream(context.Background(), &kiroRequest{})
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

	_, err := s.openStream(context.Background(), &kiroRequest{})
	require.Error(t, err)
	assert.Len(t, rt.seen, 1, "a 400 should not try further accounts")
}

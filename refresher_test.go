package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type doneObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *doneObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestAccountNeedsRefresh(t *testing.T) {
	now := time.Now()
	base := StoredAccount{ClientID: "c", ClientSecret: "s", RefreshToken: "r"}

	// No refresh token / registration -> never refresh.
	assert.False(t, accountNeedsRefresh(StoredAccount{}, now))
	assert.False(t, accountNeedsRefresh(StoredAccount{RefreshToken: "r"}, now), "missing client registration")

	// Valid, far from expiry -> no refresh.
	far := base
	far.ExpiresAt = now.Add(time.Hour).UTC().Format(time.RFC3339)
	assert.False(t, accountNeedsRefresh(far, now))

	// Within the refresh buffer -> refresh.
	soon := base
	soon.ExpiresAt = now.Add(time.Minute).UTC().Format(time.RFC3339)
	assert.True(t, accountNeedsRefresh(soon, now))

	// Expired -> refresh.
	past := base
	past.ExpiresAt = now.Add(-time.Hour).UTC().Format(time.RFC3339)
	assert.True(t, accountNeedsRefresh(past, now))

	// Unknown expiry with a usable refresh token -> refresh.
	unknown := base
	assert.True(t, accountNeedsRefresh(unknown, now))
}

func TestAccountStoreUpdateTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{
		ID: "id", ClientID: "c", ClientSecret: "sec", RefreshToken: "old-refresh",
		AccessToken: "old-access", ProfileArn: "arn:x", Email: "u@x.com",
		UserID: "d-dir.uid", CreatedAt: "1",
	}))

	require.NoError(t, s.UpdateTokens("id", "new-access", "new-refresh", "2030-01-01T00:00:00.000Z"))
	got, _ := s.Get("id")
	assert.Equal(t, "new-access", got.AccessToken)
	assert.Equal(t, "new-refresh", got.RefreshToken)
	assert.Equal(t, "2030-01-01T00:00:00.000Z", got.ExpiresAt)
	// Registration fields untouched.
	assert.Equal(t, "sec", got.ClientSecret)
	// Identity fields must survive a token refresh: userId in particular is the
	// stable dedup key and is never re-fetched by the refresh path.
	assert.Equal(t, "arn:x", got.ProfileArn)
	assert.Equal(t, "u@x.com", got.Email)
	assert.Equal(t, "d-dir.uid", got.UserID)

	// Empty refresh token preserves the existing one.
	require.NoError(t, s.UpdateTokens("id", "a2", "", "2031-01-01T00:00:00.000Z"))
	got, _ = s.Get("id")
	assert.Equal(t, "new-refresh", got.RefreshToken)

	// Reload confirms persistence.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)
	got2, _ := s2.Get("id")
	assert.Equal(t, "a2", got2.AccessToken)

	// Unknown id errors.
	assert.Error(t, s.UpdateTokens("missing", "x", "y", "z"))
}

// fakeSSOOIDC serves the ssooidc CreateToken REST-JSON endpoint at /token.
func newFakeSSOOIDC(t *testing.T, accessToken, refreshToken string, expiresIn int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken":  accessToken,
			"refreshToken": refreshToken,
			"tokenType":    "Bearer",
			"expiresIn":    expiresIn,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshAccountToken(t *testing.T) {
	srv := newFakeSSOOIDC(t, "fresh-access", "fresh-refresh", 3600)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	a := StoredAccount{ClientID: "c", ClientSecret: "s", RefreshToken: "r", Region: "us-east-1"}
	access, refresh, expiresAt, err := refreshAccountToken(context.Background(), client, a)
	require.NoError(t, err)
	assert.Equal(t, "fresh-access", access)
	assert.Equal(t, "fresh-refresh", refresh)
	assert.NotEmpty(t, expiresAt)
}

func TestRefreshAccountTokenRejectsIncomplete(t *testing.T) {
	_, _, _, err := refreshAccountToken(context.Background(), &http.Client{}, StoredAccount{})
	assert.Error(t, err)
	_, _, _, err = refreshAccountToken(context.Background(), &http.Client{}, StoredAccount{RefreshToken: "r"})
	assert.Error(t, err, "missing client registration")
}

func TestRefresherScanUpdatesStore(t *testing.T) {
	srv := newFakeSSOOIDC(t, "refreshed-access", "refreshed-refresh", 3600)
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	path := filepath.Join(t.TempDir(), "accounts.json")
	store, err := NewAccountStore(path)
	require.NoError(t, err)
	// Expired account -> should be refreshed.
	require.NoError(t, store.Add(&StoredAccount{
		ID: "stale", ClientID: "c", ClientSecret: "s", RefreshToken: "r", Region: "us-east-1",
		AccessToken: "old", ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), CreatedAt: "1",
	}))
	// Fresh account -> should be left alone.
	require.NoError(t, store.Add(&StoredAccount{
		ID: "fresh", ClientID: "c", ClientSecret: "s", RefreshToken: "r", Region: "us-east-1",
		AccessToken: "keep", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), CreatedAt: "2",
	}))

	r := newAccountRefresher(store, client, nil)
	r.scan(context.Background())

	stale, _ := store.Get("stale")
	assert.Equal(t, "refreshed-access", stale.AccessToken, "expired account refreshed")
	fresh, _ := store.Get("fresh")
	assert.Equal(t, "keep", fresh.AccessToken, "fresh account untouched")
}

func TestStoreRefreshTokenDedupsConcurrent(t *testing.T) {
	// A fake CreateToken that counts hits and blocks briefly so concurrent
	// callers overlap inside the singleflight window.
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(50 * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken": "shared-access", "refreshToken": "shared-refresh",
			"tokenType": "Bearer", "expiresIn": 3600,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acct", ClientID: "c", ClientSecret: "s", RefreshToken: "r", Region: "us-east-1", CreatedAt: "1",
	}))

	const n = 20
	var wg sync.WaitGroup
	results := make([]StoredAccount, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.RefreshToken(context.Background(), client, "acct")
		}(i)
	}
	wg.Wait()

	// Exactly one upstream CreateToken despite n concurrent callers.
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "concurrent refreshes must collapse to one CreateToken")
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.Equal(t, "shared-access", results[i].AccessToken)
		assert.Equal(t, "shared-refresh", results[i].RefreshToken)
	}
	// Persisted.
	got, _ := store.Get("acct")
	assert.Equal(t, "shared-access", got.AccessToken)
}

func TestStoreRefreshTokenCanceledContextDoesNotStartFlight(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected token refresh", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acct", ClientID: "c", ClientSecret: "s", RefreshToken: "r",
		Region: "us-east-1", CreatedAt: "1",
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.RefreshToken(ctx, client, "acct")
	require.ErrorIs(t, err, context.Canceled)
	require.Never(t, func() bool { return hits.Load() != 0 }, 100*time.Millisecond, 5*time.Millisecond)
}

func TestStoreRefreshTokenCanceledWaiterLeavesSharedRefreshRunning(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		close(started)
		<-release
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken": "fresh-access", "refreshToken": "fresh-refresh",
			"tokenType": "Bearer", "expiresIn": 3600,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acct", ClientID: "c", ClientSecret: "s", RefreshToken: "r",
		Region: "us-east-1", CreatedAt: "1",
	}))

	ownerDone := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshToken(context.Background(), client, "acct")
		ownerDone <- refreshErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared token refresh did not start")
	}

	waiterBase, cancelWaiter := context.WithCancel(context.Background())
	defer cancelWaiter()
	waiterCtx := &doneObservedContext{Context: waiterBase, observed: make(chan struct{})}
	waiterDone := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshToken(waiterCtx, client, "acct")
		waiterDone <- refreshErr
	}()
	select {
	case <-waiterCtx.observed:
		// refreshTokenAtRevision calls Done only after DoChan has joined the
		// existing flight, so cancellation below exercises a live waiter.
	case <-time.After(time.Second):
		t.Fatal("waiter did not join the shared token refresh")
	}
	cancelWaiter()
	select {
	case refreshErr := <-waiterDone:
		require.ErrorIs(t, refreshErr, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked on the shared token refresh")
	}
	assert.Equal(t, int32(1), hits.Load())

	unblock()
	select {
	case refreshErr := <-ownerDone:
		require.NoError(t, refreshErr)
	case <-time.After(time.Second):
		t.Fatal("shared token refresh did not finish after release")
	}
	got, ok := store.Get("acct")
	require.True(t, ok)
	assert.Equal(t, "fresh-access", got.AccessToken)
	assert.Equal(t, "fresh-refresh", got.RefreshToken)
}

func TestStoreRefreshTokenOutlivesInitiatingCaller(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		close(started)
		<-release
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken": "fresh-access", "refreshToken": "fresh-refresh",
			"tokenType": "Bearer", "expiresIn": 3600,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acct", ClientID: "c", ClientSecret: "s", RefreshToken: "r",
		Region: "us-east-1", CreatedAt: "1",
	}))

	initiatorCtx, cancelInitiator := context.WithCancel(context.Background())
	initiatorDone := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshToken(initiatorCtx, client, "acct")
		initiatorDone <- refreshErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shared token refresh did not start")
	}
	cancelInitiator()
	select {
	case refreshErr := <-initiatorDone:
		require.ErrorIs(t, refreshErr, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("initiating caller did not stop after cancellation")
	}

	unblock()
	require.Eventually(t, func() bool {
		got, ok := store.Get("acct")
		return ok && got.AccessToken == "fresh-access" && got.RefreshToken == "fresh-refresh"
	}, time.Second, 10*time.Millisecond, "shared refresh did not publish after its initiating caller left")
	assert.Equal(t, int32(1), hits.Load(), "caller cancellation must not start a second refresh")
}

func TestStoreRefreshTokenSharesFlightAcrossPolicyRevisions(t *testing.T) {
	var hits atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			close(started)
		}
		<-release
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken": "shared-result", "refreshToken": "shared-rotated", "expiresIn": 3600,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	unblock := cleanupTestRelease(t, release)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acct", ClientID: "client", ClientSecret: "secret",
		RefreshToken: "refresh", Region: "us-east-1", CreatedAt: "1",
	}))
	initial, ok := store.Runtime("acct")
	require.True(t, ok)

	first := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshToken(context.Background(), client, "acct")
		first <- refreshErr
	}()
	<-started

	require.NoError(t, store.SetDisabled("acct", true))
	require.NoError(t, store.SetOverageEnabled("acct", true))
	policy, ok := store.Runtime("acct")
	require.True(t, ok)
	assert.Greater(t, policy.Revision, initial.Revision)
	assert.Equal(t, initial.Credential, policy.Credential)

	second := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshToken(context.Background(), client, "acct")
		second <- refreshErr
	}()
	require.Never(t, func() bool { return hits.Load() > 1 }, 50*time.Millisecond, 5*time.Millisecond)
	unblock()
	require.NoError(t, <-first)
	require.NoError(t, <-second)

	got, ok := store.Get("acct")
	require.True(t, ok)
	assert.Equal(t, "shared-result", got.AccessToken)
	assert.Equal(t, "shared-rotated", got.RefreshToken)
	assert.Equal(t, int32(1), hits.Load())
}

func TestStoreRefreshTokenSeparatesCredentialGenerations(t *testing.T) {
	var hits atomic.Int32
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		switch hits.Add(1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			writeJSON(w, http.StatusOK, map[string]any{
				"accessToken": "old-result", "refreshToken": "old-rotated", "expiresIn": 3600,
			})
		case 2:
			close(secondStarted)
			writeJSON(w, http.StatusOK, map[string]any{
				"accessToken": "new-result", "refreshToken": "new-rotated", "expiresIn": 3600,
			})
		default:
			http.Error(w, "unexpected refresh", http.StatusInternalServerError)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	unblockFirst := cleanupTestRelease(t, releaseFirst)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acct", ClientID: "old-client", ClientSecret: "old-secret",
		RefreshToken: "old-refresh", Region: "us-east-1", CreatedAt: "1",
	}))

	firstErr := make(chan error, 1)
	go func() {
		_, refreshErr := store.RefreshToken(context.Background(), client, "acct")
		firstErr <- refreshErr
	}()
	<-firstStarted

	current, ok := store.Get("acct")
	require.True(t, ok)
	current.ClientID = "new-client"
	current.ClientSecret = "new-secret"
	current.RefreshToken = "new-refresh"
	require.NoError(t, store.ReplaceCredentials("acct", &current))

	secondResult := make(chan StoredAccount, 1)
	secondErr := make(chan error, 1)
	go func() {
		fresh, refreshErr := store.RefreshToken(context.Background(), client, "acct")
		secondResult <- fresh
		secondErr <- refreshErr
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("new credentials joined the stale refresh flight")
	}
	require.NoError(t, <-secondErr)
	assert.Equal(t, "new-result", (<-secondResult).AccessToken)

	unblockFirst()
	require.ErrorIs(t, <-firstErr, errAccountRevisionChanged)
	got, ok := store.Get("acct")
	require.True(t, ok)
	assert.Equal(t, "new-client", got.ClientID)
	assert.Equal(t, "new-result", got.AccessToken)
	assert.Equal(t, "new-rotated", got.RefreshToken)
	assert.Equal(t, int32(2), hits.Load())
}

func TestRefresherRunStopsOnContext(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	r := newAccountRefresher(store, &http.Client{}, nil)
	r.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresher did not stop on context cancel")
	}
}

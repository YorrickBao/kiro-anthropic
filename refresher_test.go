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
		AccessToken: "old-access", CreatedAt: "1",
	}))

	require.NoError(t, s.UpdateTokens("id", "new-access", "new-refresh", "2030-01-01T00:00:00.000Z"))
	got, _ := s.Get("id")
	assert.Equal(t, "new-access", got.AccessToken)
	assert.Equal(t, "new-refresh", got.RefreshToken)
	assert.Equal(t, "2030-01-01T00:00:00.000Z", got.ExpiresAt)
	// Registration fields untouched.
	assert.Equal(t, "sec", got.ClientSecret)

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

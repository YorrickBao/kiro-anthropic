package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	s, err := NewAccountStore(path)
	require.NoError(t, err)
	assert.Empty(t, s.List())

	a := &StoredAccount{
		ID:           "id-1",
		Label:        "team a",
		Provider:     "Enterprise",
		AuthMethod:   "IdC",
		Region:       "us-east-1",
		ClientID:     "cid",
		ClientSecret: "csecret",
		AccessToken:  "atoken",
		RefreshToken: "rtoken",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	require.NoError(t, s.Add(a))

	// Reload from disk into a fresh store.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)
	got, ok := s2.Get("id-1")
	require.True(t, ok)
	assert.Equal(t, "team a", got.Label)
	assert.Equal(t, "csecret", got.ClientSecret)
	assert.Equal(t, "rtoken", got.RefreshToken)
}

func TestAccountStoreRefreshIdentityOverwritesAndPersists(t *testing.T) {
	// Fake management endpoint returning fresh identity. rewriteTransport (from
	// login_test.go) redirects management.<region>.kiro.dev to this server.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"profiles": []map[string]any{{"arn": "arn:new"}},
		})
	})
	mux.HandleFunc("/getUsageLimits", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"userInfo": map[string]any{"email": "new@x.com", "userId": "user-new"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)

	// Seed with stale identity and a token valid far into the future, so
	// RefreshIdentity skips the token refresh and only re-resolves identity.
	require.NoError(t, s.Add(&StoredAccount{
		ID:          "id-1",
		Region:      "us-east-1",
		AccessToken: "atoken",
		ExpiresAt:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		ProfileArn:  "arn:old",
		Email:       "old@x.com",
		UserID:      "user-old",
		CreatedAt:   "2020-01-01T00:00:00Z",
	}))

	got, err := s.RefreshIdentity(context.Background(), client, "id-1")
	require.NoError(t, err)
	assert.Equal(t, "arn:new", got.ProfileArn)
	assert.Equal(t, "new@x.com", got.Email)
	assert.Equal(t, "user-new", got.UserID)

	// The overwrite must survive a reload from disk.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)
	reloaded, ok := s2.Get("id-1")
	require.True(t, ok)
	assert.Equal(t, "arn:new", reloaded.ProfileArn)
	assert.Equal(t, "new@x.com", reloaded.Email)
	assert.Equal(t, "user-new", reloaded.UserID)
}

// A failed identity lookup must not erase existing identity.
func TestAccountStoreRefreshIdentityKeepsExistingOnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{
		ID:          "id-1",
		Region:      "us-east-1",
		AccessToken: "atoken",
		ExpiresAt:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		ProfileArn:  "arn:old",
		Email:       "old@x.com",
		UserID:      "user-old",
		CreatedAt:   "2020-01-01T00:00:00Z",
	}))

	_, err = s.RefreshIdentity(context.Background(), client, "id-1")
	require.Error(t, err)

	got, ok := s.Get("id-1")
	require.True(t, ok)
	assert.Equal(t, "arn:old", got.ProfileArn)
	assert.Equal(t, "old@x.com", got.Email)
	assert.Equal(t, "user-old", got.UserID)
}

func TestAccountStoreImportAccounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	s, err := NewAccountStore(path)
	require.NoError(t, err)

	// Seed one account whose identity (email) will match an incoming one.
	require.NoError(t, s.Add(&StoredAccount{
		ID: "existing", Label: "keep me", Email: "a@x.com",
		RefreshToken: "old-refresh", CreatedAt: "2020-01-01T00:00:00Z",
	}))

	res, err := s.ImportAccounts([]*StoredAccount{
		// Same identity as "existing": credentials replaced in place.
		{ID: "other-id", Email: "a@x.com", RefreshToken: "new-refresh", Label: "should not overwrite"},
		// Brand new account, keeps its id.
		{ID: "brand-new", Email: "b@x.com", RefreshToken: "r2"},
		// No id: one is minted.
		{Email: "c@x.com", RefreshToken: "r3"},
		// No usable credential: skipped.
		{Email: "d@x.com"},
		// nil: skipped.
		nil,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Added)
	assert.Equal(t, 1, res.Replaced)

	// Reload from disk to confirm persistence.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)

	kept, ok := s2.Get("existing")
	require.True(t, ok)
	assert.Equal(t, "new-refresh", kept.RefreshToken, "credentials replaced in place")
	assert.Equal(t, "keep me", kept.Label, "label preserved")
	assert.Equal(t, "2020-01-01T00:00:00Z", kept.CreatedAt, "createdAt preserved")
	_, gone := s2.Get("other-id")
	assert.False(t, gone, "duplicate must not create a second account")

	brand, ok := s2.Get("brand-new")
	require.True(t, ok)
	assert.Equal(t, "r2", brand.RefreshToken)
	assert.NotEmpty(t, brand.CreatedAt, "createdAt minted when missing")

	assert.Len(t, s2.List(), 3, "existing + brand-new + minted-id; empty/nil skipped")
}

func TestAccountStoreImportMintsIDOnCollision(t *testing.T) {
	dir := t.TempDir()
	s, err := NewAccountStore(filepath.Join(dir, "accounts.json"))
	require.NoError(t, err)

	require.NoError(t, s.Add(&StoredAccount{ID: "dup", Email: "a@x.com", RefreshToken: "r1", CreatedAt: "2020"}))

	// Different identity but a colliding id: must mint a new id, not clobber.
	res, err := s.ImportAccounts([]*StoredAccount{{ID: "dup", Email: "z@x.com", RefreshToken: "r9"}})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Added)

	orig, ok := s.Get("dup")
	require.True(t, ok)
	assert.Equal(t, "a@x.com", orig.Email, "original id owner untouched")
	assert.Len(t, s.List(), 2)
}

func TestAccountStoreFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "accounts.json")

	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{ID: "x", CreatedAt: "2020"}))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "accounts file must be 0600")

	di, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm(), "accounts dir must be 0700")
}

func TestAccountStoreUpdateLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{ID: "id", Label: "old", CreatedAt: "1"}))

	require.NoError(t, s.UpdateLabel("id", "new note"))
	got, _ := s.Get("id")
	assert.Equal(t, "new note", got.Label)

	// Persisted across reload.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)
	got2, _ := s2.Get("id")
	assert.Equal(t, "new note", got2.Label)

	assert.Error(t, s.UpdateLabel("missing", "x"))
}

func TestAccountStoreUpdateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{
		ID: "id", ClientID: "cid", RefreshToken: "ref", CreatedAt: "1",
		// profileArn and Email both empty — as if imported with network failure.
	}))

	// Backfill all three fields.
	require.NoError(t, s.UpdateIdentity("id", "arn:resolved", "user@example.com", "d-dir.uid"))
	got, _ := s.Get("id")
	assert.Equal(t, "arn:resolved", got.ProfileArn)
	assert.Equal(t, "user@example.com", got.Email)
	assert.Equal(t, "d-dir.uid", got.UserID)

	// Partial update: empty values are ignored, not erased.
	require.NoError(t, s.UpdateIdentity("id", "arn:new", "", ""))
	got, _ = s.Get("id")
	assert.Equal(t, "arn:new", got.ProfileArn)
	assert.Equal(t, "user@example.com", got.Email, "email must not be erased by empty value")
	assert.Equal(t, "d-dir.uid", got.UserID, "userId must not be erased by empty value")

	// Persisted across reload.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)
	got2, _ := s2.Get("id")
	assert.Equal(t, "arn:new", got2.ProfileArn)
	assert.Equal(t, "user@example.com", got2.Email)
	assert.Equal(t, "d-dir.uid", got2.UserID)

	assert.Error(t, s.UpdateIdentity("missing", "arn", "e", "u"))
}

func TestAccountStoreSetDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{ID: "id", ClientID: "c", RefreshToken: "r", CreatedAt: "1"}))

	// Default state is enabled (legacy accounts stay in the pool).
	got, _ := s.Get("id")
	assert.False(t, got.Disabled)

	// Park the account out of the pool.
	require.NoError(t, s.SetDisabled("id", true))
	got, _ = s.Get("id")
	assert.True(t, got.Disabled)

	// Persisted across reload.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)
	got2, _ := s2.Get("id")
	assert.True(t, got2.Disabled)

	// Re-enable and verify the toggle is reversible.
	require.NoError(t, s2.SetDisabled("id", false))
	got2, _ = s2.Get("id")
	assert.False(t, got2.Disabled)

	assert.Error(t, s.SetDisabled("missing", true), "unknown id -> error")
}

func TestAccountStoreSetOverageEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{ID: "id", ClientID: "c", RefreshToken: "r", CreatedAt: "1"}))

	// Default is off: legacy accounts stay strictly within base credit.
	got, _ := s.Get("id")
	assert.False(t, got.OverageEnabled)

	// Opt the account into overage.
	require.NoError(t, s.SetOverageEnabled("id", true))
	got, _ = s.Get("id")
	assert.True(t, got.OverageEnabled)

	// Persisted across reload.
	s2, err := NewAccountStore(path)
	require.NoError(t, err)
	got2, _ := s2.Get("id")
	assert.True(t, got2.OverageEnabled)

	// Re-disable and verify the toggle is reversible.
	require.NoError(t, s2.SetOverageEnabled("id", false))
	got2, _ = s2.Get("id")
	assert.False(t, got2.OverageEnabled)

	assert.Error(t, s.SetOverageEnabled("missing", true), "unknown id -> error")
}

func TestAccountStoreRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)

	require.NoError(t, s.Add(&StoredAccount{ID: "a", CreatedAt: "1"}))
	require.NoError(t, s.Add(&StoredAccount{ID: "b", CreatedAt: "2"}))
	assert.Len(t, s.List(), 2)

	require.NoError(t, s.Remove("a"))
	_, ok := s.Get("a")
	assert.False(t, ok)
	assert.Len(t, s.List(), 1)

	// Removing a missing id is a no-op success.
	require.NoError(t, s.Remove("missing"))
}

func TestAccountStoreListOrdered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{ID: "b", CreatedAt: "2020-01-02T00:00:00Z"}))
	require.NoError(t, s.Add(&StoredAccount{ID: "a", CreatedAt: "2020-01-01T00:00:00Z"}))

	list := s.List()
	require.Len(t, list, 2)
	assert.Equal(t, "a", list[0].ID, "ordered by createdAt")
	assert.Equal(t, "b", list[1].ID)
}

func TestStoredAccountViewRedacts(t *testing.T) {
	a := StoredAccount{
		ID:           "id",
		AccessToken:  "abcdefghijklmnopqrstuvwxyz",
		RefreshToken: "secret-refresh",
		UserID:       "d-dir.uid",
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	v := a.view()
	assert.NotEqual(t, a.AccessToken, v["access_token"], "access token must be masked")
	assert.Equal(t, true, v["has_refresh"])
	assert.Equal(t, "d-dir.uid", v["user_id"], "userId is surfaced for the admin page")
	assert.NotContains(t, v, "refresh_token", "raw refresh token must not be exposed")
	assert.NotContains(t, v, "client_secret", "client secret must not be exposed")
	assert.Equal(t, "valid", v["expiry_state"])
	assert.Equal(t, false, v["disabled"], "disabled is surfaced for the admin toggle")
	assert.Equal(t, false, v["overage_enabled"], "overage_enabled is surfaced for the admin toggle")
}

func TestStoredAccountExpiryState(t *testing.T) {
	past := StoredAccount{ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	assert.Equal(t, "expired", past.view()["expiry_state"])

	soon := StoredAccount{ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}
	assert.Equal(t, "expiring soon", soon.view()["expiry_state"])

	unknown := StoredAccount{}
	assert.Equal(t, "unknown", unknown.view()["expiry_state"])
}

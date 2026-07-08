package main

import (
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
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	v := a.view()
	assert.NotEqual(t, a.AccessToken, v["access_token"], "access token must be masked")
	assert.Equal(t, true, v["has_refresh"])
	assert.NotContains(t, v, "refresh_token", "raw refresh token must not be exposed")
	assert.NotContains(t, v, "client_secret", "client secret must not be exposed")
	assert.Equal(t, "valid", v["expiry_state"])
}

func TestStoredAccountExpiryState(t *testing.T) {
	past := StoredAccount{ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	assert.Equal(t, "expired", past.view()["expiry_state"])

	soon := StoredAccount{ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}
	assert.Equal(t, "expiring soon", soon.view()["expiry_state"])

	unknown := StoredAccount{}
	assert.Equal(t, "unknown", unknown.view()["expiry_state"])
}

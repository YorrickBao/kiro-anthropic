package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeJSONFile writes v as JSON to path.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
}

func TestImportLocalCredentialsWithCompanion(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "kiro-auth-token.json")
	writeJSONFile(t, tokenFile, map[string]any{
		"accessToken":  "acc",
		"refreshToken": "ref",
		"clientIdHash": "hash123",
		"authMethod":   "IdC",
		"provider":     "Enterprise",
		"region":       "us-east-1",
		"profileArn":   "arn:aws:codewhisperer:us-east-1:1:profile/X",
		"expiresAt":    "2030-01-01T00:00:00.000Z",
	})
	writeJSONFile(t, filepath.Join(dir, "hash123.json"), map[string]any{
		"clientId":     "cid",
		"clientSecret": "csecret",
	})

	acct, err := importLocalCredentials(context.Background(), &http.Client{}, tokenFile)
	require.NoError(t, err)
	assert.Equal(t, "ref", acct.RefreshToken)
	assert.Equal(t, "cid", acct.ClientID)
	assert.Equal(t, "csecret", acct.ClientSecret)
	assert.Equal(t, "Enterprise", acct.Provider)
	assert.Equal(t, "IdC", acct.AuthMethod)
	assert.Equal(t, "arn:aws:codewhisperer:us-east-1:1:profile/X", acct.ProfileArn)
	assert.NotEmpty(t, acct.ID)
}

func TestImportLocalCredentialsScanFallback(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "kiro-auth-token.json")
	// clientIdHash points nowhere; registration must be found by directory scan.
	writeJSONFile(t, tokenFile, map[string]any{
		"accessToken":  "acc",
		"refreshToken": "ref",
		"clientIdHash": "missing",
		"region":       "us-east-1",
		"profileArn":   "arn:x",
	})
	writeJSONFile(t, filepath.Join(dir, "some-other-reg.json"), map[string]any{
		"clientId":     "scanned-cid",
		"clientSecret": "scanned-secret",
	})

	acct, err := importLocalCredentials(context.Background(), &http.Client{}, tokenFile)
	require.NoError(t, err)
	assert.Equal(t, "scanned-cid", acct.ClientID)
	assert.Equal(t, "scanned-secret", acct.ClientSecret)
}

func TestImportLocalCredentialsNoRefreshToken(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "kiro-auth-token.json")
	writeJSONFile(t, tokenFile, map[string]any{"accessToken": "acc"})

	_, err := importLocalCredentials(context.Background(), &http.Client{}, tokenFile)
	assert.Error(t, err)
}

func TestImportLocalCredentialsMissingFile(t *testing.T) {
	_, err := importLocalCredentials(context.Background(), &http.Client{}, filepath.Join(t.TempDir(), "nope.json"))
	assert.Error(t, err)
}

func TestFindByRefreshTokenDedup(t *testing.T) {
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{ID: "a", ClientID: "cid", RefreshToken: "ref", CreatedAt: "1"}))

	id, ok := s.FindByRefreshToken("cid", "ref")
	assert.True(t, ok)
	assert.Equal(t, "a", id)

	_, ok = s.FindByRefreshToken("cid", "other")
	assert.False(t, ok)
	_, ok = s.FindByRefreshToken("other", "ref")
	assert.False(t, ok, "clientId must also match")
	_, ok = s.FindByRefreshToken("cid", "")
	assert.False(t, ok, "empty refresh token never matches")
}

func TestAdminImportEndpointAndDedup(t *testing.T) {
	// Build a token cache to import from.
	cacheDir := t.TempDir()
	tokenFile := filepath.Join(cacheDir, "kiro-auth-token.json")
	writeJSONFile(t, tokenFile, map[string]any{
		"accessToken": "acc", "refreshToken": "ref", "clientIdHash": "h",
		"region": "us-east-1", "provider": "Enterprise", "authMethod": "IdC",
		"profileArn": "arn:x",
	})
	writeJSONFile(t, filepath.Join(cacheDir, "h.json"), map[string]any{"clientId": "cid", "clientSecret": "sec"})

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	s := &Server{cfg: &Config{TokenFile: tokenFile}, accounts: store, login: newLoginManager(&http.Client{})}
	h := s.AdminHandler()

	// First import creates the account.
	rr := doAdmin(h, http.MethodPost, "/api/accounts/import", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var first struct {
		OK             bool   `json:"ok"`
		ID             string `json:"id"`
		AlreadyPresent bool   `json:"already_present"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &first))
	assert.True(t, first.OK)
	assert.False(t, first.AlreadyPresent)
	assert.Len(t, store.List(), 1)

	// Second import is a no-op dedup.
	rr2 := doAdmin(h, http.MethodPost, "/api/accounts/import", "")
	require.Equal(t, http.StatusOK, rr2.Code)
	var second struct {
		AlreadyPresent bool `json:"already_present"`
	}
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &second))
	assert.True(t, second.AlreadyPresent)
	assert.Len(t, store.List(), 1, "no duplicate created")
}

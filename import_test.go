package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestFetchAccountEmail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/getUsageLimits", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		writeJSON(w, http.StatusOK, map[string]any{
			"userInfo": map[string]any{"email": "user@example.com"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	email := fetchAccountEmail(context.Background(), client, "us-east-1", "arn:x", "tok")
	assert.Equal(t, "user@example.com", email)
}

func TestFetchAccountEmailBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	// Failure returns empty string, not an error.
	assert.Equal(t, "", fetchAccountEmail(context.Background(), client, "us-east-1", "", "tok"))
}

func TestFindDuplicate(t *testing.T) {
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{
		ID: "a", ClientID: "cid", RefreshToken: "ref",
		ProfileArn: "arn:1", Email: "u@x.com", CreatedAt: "1",
	}))

	// Same account via a different path (new clientId/refreshToken) but same
	// profileArn -> duplicate.
	id, ok := s.FindDuplicate(StoredAccount{ClientID: "other", RefreshToken: "other", ProfileArn: "arn:1"})
	assert.True(t, ok)
	assert.Equal(t, "a", id)

	// Match by email when profileArn differs/absent.
	id, ok = s.FindDuplicate(StoredAccount{Email: "u@x.com"})
	assert.True(t, ok)
	assert.Equal(t, "a", id)

	// Different account -> no match.
	_, ok = s.FindDuplicate(StoredAccount{ProfileArn: "arn:2", Email: "other@x.com"})
	assert.False(t, ok)

	// No identity fields on the candidate: fall back to clientId alone. The
	// refreshToken rotates across re-imports, so it must NOT be part of the key.
	id, ok = s.FindDuplicate(StoredAccount{ClientID: "cid", RefreshToken: "ref"})
	assert.True(t, ok)
	assert.Equal(t, "a", id)
	id, ok = s.FindDuplicate(StoredAccount{ClientID: "cid", RefreshToken: "rotated-different"})
	assert.True(t, ok, "same clientId, rotated refreshToken -> still a duplicate")
	assert.Equal(t, "a", id)
	// Different clientId (e.g. another machine) -> no match via this fallback.
	_, ok = s.FindDuplicate(StoredAccount{ClientID: "other-cid", RefreshToken: "ref"})
	assert.False(t, ok)
}

func TestFindDuplicateByClientIDWhenProfileArnEmpty(t *testing.T) {
	// Reproduces the bug: an import whose profileArn/email lookup failed (e.g.
	// --proxy none) stores neither, and its refreshToken rotates between
	// restarts. Dedup must still recognise it by clientId alone.
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{
		ID: "first", ClientID: "C1", ClientSecret: "s", RefreshToken: "OLD",
		CreatedAt: "1",
	}))

	id, ok := s.FindDuplicate(StoredAccount{ClientID: "C1", ClientSecret: "s", RefreshToken: "NEW"})
	assert.True(t, ok, "same machine re-import must dedup despite rotated refreshToken")
	assert.Equal(t, "first", id)
}

func TestReplaceCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	s, err := NewAccountStore(path)
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{
		ID: "a", Label: "keep me", ProfileArn: "arn:1",
		AccessToken: "old", RefreshToken: "oldref", CreatedAt: "1",
	}))

	err = s.ReplaceCredentials("a", &StoredAccount{
		AccessToken: "new", RefreshToken: "newref", ClientID: "c2", Email: "e@x.com", ProfileArn: "arn:1",
	})
	require.NoError(t, err)

	got, _ := s.Get("a")
	assert.Equal(t, "keep me", got.Label, "label preserved")
	assert.Equal(t, "1", got.CreatedAt, "createdAt preserved")
	assert.Equal(t, "new", got.AccessToken)
	assert.Equal(t, "newref", got.RefreshToken)
	assert.Equal(t, "e@x.com", got.Email)

	assert.Error(t, s.ReplaceCredentials("missing", &StoredAccount{}))
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

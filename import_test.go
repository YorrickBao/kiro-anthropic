package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

// TestImportLocalCredentialsRejectsNoIdentity reproduces the duplicate-account
// bug: when the token file has no profileArn and the management endpoint is
// unreachable (returning non-2xx), both profileArn and email resolve to empty.
// The import must be rejected rather than creating a blank "imported" entry
// that dedup can never recognise on the next import.
func TestImportLocalCredentialsRejectsNoIdentity(t *testing.T) {
	// Management endpoint returns 403 for every call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "kiro-auth-token.json")
	writeJSONFile(t, tokenFile, map[string]any{
		"accessToken":  "acc",
		"refreshToken": "ref",
		"clientIdHash": "hash123",
		"authMethod":   "IdC",
		"provider":     "Enterprise",
		"region":       "us-east-1",
		// No profileArn — forces resolveProfileArn + fetchAccountEmail network calls.
		"expiresAt":    "2030-01-01T00:00:00.000Z",
	})
	writeJSONFile(t, filepath.Join(dir, "hash123.json"), map[string]any{
		"clientId":     "cid",
		"clientSecret": "csecret",
	})

	_, err := importLocalCredentials(context.Background(), client, tokenFile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not resolve profileArn or email")
}

// TestImportLocalCredentialsRefreshesExpiredToken verifies that when the cached
// access token is expired, importLocalCredentials refreshes it via SSO-OIDC
// before resolving profileArn/email. The management endpoint should see the
// refreshed access token, not the stale one from the cache.
func TestImportLocalCredentialsRefreshesExpiredToken(t *testing.T) {
	var tokenHits int32
	mux := http.NewServeMux()
	// SSO-OIDC CreateToken endpoint.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenHits, 1)
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken":  "refreshed-access",
			"refreshToken": "refreshed-refresh",
			"tokenType":    "Bearer",
			"expiresIn":    3600,
		})
	})
	// management endpoint — ListAvailableProfiles (POST /).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// Verify the refreshed token is used, not the stale one.
		assert.Equal(t, "Bearer refreshed-access", r.Header.Get("Authorization"))
		writeJSON(w, http.StatusOK, map[string]any{
			"profiles": []map[string]any{{"arn": "arn:aws:codewhisperer:us-east-1:1:profile/imported"}},
		})
	})
	// management endpoint — getUsageLimits.
	mux.HandleFunc("/getUsageLimits", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer refreshed-access", r.Header.Get("Authorization"))
		writeJSON(w, http.StatusOK, map[string]any{
			"userInfo": map[string]any{"email": "imported@example.com"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "kiro-auth-token.json")
	writeJSONFile(t, tokenFile, map[string]any{
		"accessToken":  "stale-access",
		"refreshToken": "orig-refresh",
		"clientIdHash": "hash123",
		"authMethod":   "IdC",
		"provider":     "Enterprise",
		"region":       "us-east-1",
		// No profileArn; access token expired.
		"expiresAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	writeJSONFile(t, filepath.Join(dir, "hash123.json"), map[string]any{
		"clientId":     "cid",
		"clientSecret": "csecret",
	})

	acct, err := importLocalCredentials(context.Background(), client, tokenFile)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&tokenHits), "expired token must trigger exactly one refresh")
	assert.Equal(t, "refreshed-access", acct.AccessToken)
	assert.Equal(t, "refreshed-refresh", acct.RefreshToken)
	assert.Equal(t, "arn:aws:codewhisperer:us-east-1:1:profile/imported", acct.ProfileArn)
	assert.Equal(t, "imported@example.com", acct.Email)
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

	// Candidate has NO identity but the stored account HAS identity. clientId
	// alone must NOT match here: two different AWS accounts signed in via the
	// same IdC start URL can share an OIDC client registration, so a bare
	// clientId match would wrongly merge distinct users. The refreshToken
	// rotates across re-imports, so it must NOT be part of the key either.
	_, ok = s.FindDuplicate(StoredAccount{ClientID: "cid", RefreshToken: "ref"})
	assert.False(t, ok,
		"candidate without identity must not match a stored account that HAS identity")
	_, ok = s.FindDuplicate(StoredAccount{ClientID: "cid", RefreshToken: "rotated-different"})
	assert.False(t, ok, "same clientId, rotated refreshToken -> still no match without identity")
	// Different clientId -> also no match.
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

// TestFindDuplicateClientIDDoesNotOverrideIdentity reproduces the cross-account
// overwrite bug: two different AWS accounts signed in via the same IdC start URL
// can share the same OIDC clientId (AWS reuses the client registration for the
// same start URL). When the second account's profileArn/email lookup fails,
// path 3 (clientId fallback) must NOT match it against the first account — they
// are different accounts with different emails.
func TestFindDuplicateClientIDDoesNotOverrideIdentity(t *testing.T) {
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	// First account: fully resolved identity.
	require.NoError(t, s.Add(&StoredAccount{
		ID: "acct-a", ClientID: "shared-cid", RefreshToken: "r1",
		ProfileArn: "arn:account-a", Email: "alice@example.com", CreatedAt: "1",
	}))

	// Second account (different email, different profileArn) but same clientId
	// because the IdC start URL is the same. profileArn/email failed to resolve
	// (empty) on this candidate — path 3 would wrongly match acct-a.
	_, ok := s.FindDuplicate(StoredAccount{
		ClientID: "shared-cid", RefreshToken: "r2",
		// ProfileArn and Email both empty.
	})
	assert.False(t, ok,
		"different account with empty identity must not match by clientId alone "+
			"when the stored account HAS identity — it is a different person")
}

// TestFindDuplicateBackfillsExistingEmptyIdentity covers the scenario where an
// account was first stored with empty identity (profileArn/email lookup failed
// during import), and later the same account signs in via OAuth with a
// resolved profileArn. The candidate's clientId matches the stored account, and
// the candidate now carries the profileArn the stored one is missing. Dedup
// should match so the identity gets backfilled via ReplaceCredentials, rather
// than creating a duplicate.
func TestFindDuplicateBackfillsExistingEmptyIdentity(t *testing.T) {
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	// Stored account: identity empty, only clientId is set (imported with
	// network failure).
	require.NoError(t, s.Add(&StoredAccount{
		ID: "blank", ClientID: "C1", ClientSecret: "s", RefreshToken: "old",
		CreatedAt: "1",
	}))

	// Same account now arrives with profileArn resolved (via OAuth sign-in).
	id, ok := s.FindDuplicate(StoredAccount{
		ClientID: "C1", RefreshToken: "new",
		ProfileArn: "arn:now-resolved", Email: "user@example.com",
	})
	assert.True(t, ok, "candidate with identity + matching clientId should dedup "+
		"against a stored account whose identity is still empty")
	assert.Equal(t, "blank", id)
}

// TestFindDuplicateEmailBackfillsExistingEmptyIdentity is the email-only variant
// of the backfill scenario: stored account has empty identity, candidate arrives
// with only email (profileArn still empty) and matching clientId.
func TestFindDuplicateEmailBackfillsExistingEmptyIdentity(t *testing.T) {
	s, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, s.Add(&StoredAccount{
		ID: "blank", ClientID: "C1", ClientSecret: "s", RefreshToken: "old",
		CreatedAt: "1",
	}))

	id, ok := s.FindDuplicate(StoredAccount{
		ClientID: "C1", RefreshToken: "new",
		Email: "user@example.com",
		// ProfileArn still empty.
	})
	assert.True(t, ok, "candidate with email + matching clientId should dedup "+
		"against a stored account whose identity is still empty")
	assert.Equal(t, "blank", id)
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

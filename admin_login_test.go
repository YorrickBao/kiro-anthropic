package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAdminTestServer builds a Server with an account store and a login manager
// wired to the fake OIDC endpoint, plus its admin handler.
func newAdminTestServer(t *testing.T) (*Server, http.Handler, *fakeOIDC) {
	t.Helper()
	fake := newFakeOIDC(t)
	target, err := url.Parse(fake.srv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}

	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)

	s := &Server{
		cfg: &Config{}, accounts: store, login: newLoginManager(client),
		selector:    newAccountSelector(store, client),
		modelsCache: map[string]modelsCacheEntry{},
		usageCache:  map[string]usageCacheEntry{},
	}
	return s, s.AdminHandler(), fake
}

// doAdmin performs a loopback request against the admin handler.
func doAdmin(h http.Handler, method, target string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = "localhost:27890"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAdminLoginFlowEndToEnd(t *testing.T) {
	s, h, fake := newAdminTestServer(t)

	// 1. Start login.
	rr := doAdmin(h, http.MethodPost, "/api/login/start",
		`{"start_url":"https://org.awsapps.com/start","region":"us-east-1","label":"team a"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var startResp struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &startResp))
	require.NotEmpty(t, startResp.AuthorizeURL)

	// redirect_uri must be derived from the (loopback) Host header.
	au, err := url.Parse(startResp.AuthorizeURL)
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:27890/oauth/callback", au.Query().Get("redirect_uri"))
	state := au.Query().Get("state")
	require.NotEmpty(t, state)
	_ = fake

	// 2. Callback with the code.
	cb := doAdmin(h, http.MethodGet, "/oauth/callback?code=the-code&state="+state, "")
	require.Equal(t, http.StatusOK, cb.Code, cb.Body.String())
	assert.Contains(t, cb.Body.String(), "successfully")

	// 3. Account persisted and listed (redacted).
	list := doAdmin(h, http.MethodGet, "/api/accounts.json", "")
	require.Equal(t, http.StatusOK, list.Code)
	var listResp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &listResp))
	require.Len(t, listResp.Accounts, 1)
	assert.Equal(t, "team a", listResp.Accounts[0]["label"])
	assert.NotContains(t, list.Body.String(), "test-refresh", "refresh token must not leak")
	assert.NotContains(t, list.Body.String(), "test-client-secret", "client secret must not leak")

	// 4. Delete it.
	id, _ := listResp.Accounts[0]["id"].(string)
	require.NotEmpty(t, id)
	del := doAdmin(h, http.MethodPost, "/api/accounts/delete", `{"id":"`+id+`"}`)
	require.Equal(t, http.StatusOK, del.Code)
	assert.Empty(t, s.accounts.List())
}

func TestAdminAccountLabelUpdate(t *testing.T) {
	s, h, _ := newAdminTestServer(t)
	require.NoError(t, s.accounts.Add(&StoredAccount{ID: "acc1", Label: "old", CreatedAt: "1"}))

	rr := doAdmin(h, http.MethodPost, "/api/accounts/label", `{"id":"acc1","label":"prod team"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	got, _ := s.accounts.Get("acc1")
	assert.Equal(t, "prod team", got.Label)

	// Unknown id -> 404.
	miss := doAdmin(h, http.MethodPost, "/api/accounts/label", `{"id":"nope","label":"x"}`)
	assert.Equal(t, http.StatusNotFound, miss.Code)

	// Missing id -> 400.
	bad := doAdmin(h, http.MethodPost, "/api/accounts/label", `{"label":"x"}`)
	assert.Equal(t, http.StatusBadRequest, bad.Code)
}

func TestAdminAccountDisable(t *testing.T) {
	s, h, _ := newAdminTestServer(t)
	require.NoError(t, s.accounts.Add(&StoredAccount{ID: "acc1", CreatedAt: "1"}))

	// Park out of the pool.
	rr := doAdmin(h, http.MethodPost, "/api/accounts/disable", `{"id":"acc1","disabled":true}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp struct {
		OK       bool `json:"ok"`
		Disabled bool `json:"disabled"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.True(t, resp.Disabled)
	got, _ := s.accounts.Get("acc1")
	assert.True(t, got.Disabled, "store reflects the disabled flag")

	// Re-enable.
	rr2 := doAdmin(h, http.MethodPost, "/api/accounts/disable", `{"id":"acc1","disabled":false}`)
	require.Equal(t, http.StatusOK, rr2.Code)
	got, _ = s.accounts.Get("acc1")
	assert.False(t, got.Disabled, "toggle is reversible")

	// Unknown id -> 404.
	miss := doAdmin(h, http.MethodPost, "/api/accounts/disable", `{"id":"nope","disabled":true}`)
	assert.Equal(t, http.StatusNotFound, miss.Code)

	// Missing id -> 400.
	bad := doAdmin(h, http.MethodPost, "/api/accounts/disable", `{"disabled":true}`)
	assert.Equal(t, http.StatusBadRequest, bad.Code)
}

func TestAdminAccountRefresh(t *testing.T) {
	s, h, _ := newAdminTestServer(t)
	require.NoError(t, s.accounts.Add(&StoredAccount{
		ID: "a1", ClientID: "c", ClientSecret: "s", RefreshToken: "old", Region: "us-east-1",
		AccessToken: "stale", CreatedAt: "1",
	}))
	require.NoError(t, s.accounts.Add(&StoredAccount{
		ID: "a2", ClientID: "c", ClientSecret: "s", RefreshToken: "old", Region: "us-east-1",
		AccessToken: "stale", CreatedAt: "2",
	}))

	// Refresh a single account by id.
	one := doAdmin(h, http.MethodPost, "/api/accounts/refresh", `{"id":"a1"}`)
	require.Equal(t, http.StatusOK, one.Code, one.Body.String())
	var oneResp struct {
		Refreshed, Total int
		Results          []map[string]any
	}
	require.NoError(t, json.Unmarshal(one.Body.Bytes(), &oneResp))
	assert.Equal(t, 1, oneResp.Refreshed)
	assert.Equal(t, 1, oneResp.Total)
	got, _ := s.accounts.Get("a1")
	assert.Equal(t, "test-access", got.AccessToken, "token refreshed from fake OIDC")
	still, _ := s.accounts.Get("a2")
	assert.Equal(t, "stale", still.AccessToken, "other account untouched")

	// Unknown id -> 404.
	miss := doAdmin(h, http.MethodPost, "/api/accounts/refresh", `{"id":"nope"}`)
	assert.Equal(t, http.StatusNotFound, miss.Code)

	// Empty body -> refresh all.
	all := doAdmin(h, http.MethodPost, "/api/accounts/refresh", `{}`)
	require.Equal(t, http.StatusOK, all.Code, all.Body.String())
	var allResp struct{ Refreshed, Total int }
	require.NoError(t, json.Unmarshal(all.Body.Bytes(), &allResp))
	assert.Equal(t, 2, allResp.Total)
	assert.Equal(t, 2, allResp.Refreshed)
	g2, _ := s.accounts.Get("a2")
	assert.Equal(t, "test-access", g2.AccessToken)
}

func TestAdminStatusPerAccount(t *testing.T) {
	s, h, _ := newAdminTestServer(t)
	require.NoError(t, s.accounts.Add(&StoredAccount{
		ID: "a1", Email: "a@x.com", Label: "team", Provider: "Enterprise",
		Region: "us-east-1", CreatedAt: "1",
	}))

	rr := doAdmin(h, http.MethodGet, "/api/status.json", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Len(t, resp.Accounts, 1)
	assert.Equal(t, "a@x.com", resp.Accounts[0]["email"])
	assert.Equal(t, "team", resp.Accounts[0]["label"])
	// Usage present as an inline object (error is fine; the fake has no usage endpoint).
	assert.Contains(t, resp.Accounts[0], "usage")
	// Secrets never leak.
	assert.NotContains(t, rr.Body.String(), "client_secret")
	assert.NotContains(t, rr.Body.String(), "refresh_token")
}

func TestAdminAccountReorderEndpoint(t *testing.T) {
	s, h, _ := newAdminTestServer(t)
	require.NoError(t, s.accounts.Add(&StoredAccount{ID: "a1", CreatedAt: "2020-01-01T00:00:00Z"}))
	require.NoError(t, s.accounts.Add(&StoredAccount{ID: "a2", CreatedAt: "2020-01-02T00:00:00Z"}))
	require.NoError(t, s.accounts.Add(&StoredAccount{ID: "a3", CreatedAt: "2020-01-03T00:00:00Z"}))

	listIDs := func(target string) []string {
		rr := doAdmin(h, http.MethodGet, target, "")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var resp struct {
			Accounts []map[string]any `json:"accounts"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		ids := make([]string, 0, len(resp.Accounts))
		for _, a := range resp.Accounts {
			ids = append(ids, a["id"].(string))
		}
		return ids
	}

	// Before any reorder both surfaces follow creation-time order.
	assert.Equal(t, []string{"a1", "a2", "a3"}, listIDs("/api/accounts.json"))
	assert.Equal(t, []string{"a1", "a2", "a3"}, listIDs("/api/status.json"))

	rr := doAdmin(h, http.MethodPost, "/api/accounts/reorder", `{"ids":["a3","a1","a2"]}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp struct {
		OK bool `json:"ok"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.True(t, resp.OK)

	// The stored list and the status panel both reflect the new display order.
	assert.Equal(t, []string{"a3", "a1", "a2"}, listIDs("/api/accounts.json"))
	assert.Equal(t, []string{"a3", "a1", "a2"}, listIDs("/api/status.json"))

	// Bad body -> 400, empty ids -> 400, unknown id -> 404.
	assert.Equal(t, http.StatusBadRequest, doAdmin(h, http.MethodPost, "/api/accounts/reorder", `{`).Code)
	assert.Equal(t, http.StatusBadRequest, doAdmin(h, http.MethodPost, "/api/accounts/reorder", `{"ids":[]}`).Code)
	assert.Equal(t, http.StatusNotFound, doAdmin(h, http.MethodPost, "/api/accounts/reorder", `{"ids":["a1","ghost"]}`).Code)
}

func TestAdminCallbackStateMismatch(t *testing.T) {
	_, h, _ := newAdminTestServer(t)
	cb := doAdmin(h, http.MethodGet, "/oauth/callback?code=x&state=unknown", "")
	assert.Equal(t, http.StatusBadRequest, cb.Code)
	assert.Contains(t, cb.Body.String(), "failed")
}

func TestAdminCallbackProviderError(t *testing.T) {
	_, h, _ := newAdminTestServer(t)
	cb := doAdmin(h, http.MethodGet, "/oauth/callback?error=access_denied&error_description=nope", "")
	assert.Equal(t, http.StatusBadRequest, cb.Code)
	body := cb.Body.String()
	assert.Contains(t, body, "access_denied")
}

func TestAdminLoginStartRejectsNonLoopbackPeer(t *testing.T) {
	_, h, _ := newAdminTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/login/start", strings.NewReader(`{"start_url":"https://x/start"}`))
	req.RemoteAddr = "8.8.8.8:1234"
	req.Host = "localhost:27890"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestAdminAccountsListEmpty(t *testing.T) {
	_, h, _ := newAdminTestServer(t)
	rr := doAdmin(h, http.MethodGet, "/api/accounts.json", "")
	require.Equal(t, http.StatusOK, rr.Code)
	b, _ := io.ReadAll(rr.Body)
	assert.Contains(t, string(b), `"accounts"`)
}

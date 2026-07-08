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

	s := &Server{cfg: &Config{}, accounts: store, login: newLoginManager(client)}
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

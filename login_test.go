package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOIDC is a stand-in for oidc.<region>.amazonaws.com. It records the last
// register/token request bodies and returns canned successful responses.
type fakeOIDC struct {
	srv          *httptest.Server
	registerBody map[string]any
	tokenBody    map[string]any
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	f := &fakeOIDC{}
	mux := http.NewServeMux()
	mux.HandleFunc("/client/register", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &f.registerBody)
		writeJSON(w, http.StatusOK, map[string]any{
			"clientId":              "test-client-id",
			"clientSecret":          "test-client-secret",
			"clientSecretExpiresAt": 1234567890,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &f.tokenBody)
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken":  "test-access",
			"refreshToken": "test-refresh",
			"expiresIn":    3600,
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newTestLoginManager returns a loginManager whose OIDC base is redirected to
// the fake server by using a custom transport that rewrites the host.
func newTestLoginManager(t *testing.T, fakeURL string) *loginManager {
	t.Helper()
	target, err := url.Parse(fakeURL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}
	return newLoginManager(client)
}

// rewriteTransport redirects every request to the fake server, preserving the
// path, so calls to https://oidc.<region>.amazonaws.com/... hit httptest.
type rewriteTransport struct {
	host   string
	scheme string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.scheme
	req.URL.Host = rt.host
	return http.DefaultTransport.RoundTrip(req)
}

func TestStartLoginRegistersAndBuildsAuthorizeURL(t *testing.T) {
	fake := newFakeOIDC(t)
	m := newTestLoginManager(t, fake.srv.URL)

	redirectURI := "http://localhost:27890/oauth/callback"
	authURL, state, err := m.startLogin(context.Background(),
		"https://org.awsapps.com/start", "us-east-1", "team", redirectURI)
	require.NoError(t, err)
	require.NotEmpty(t, state)

	// Registration body carries the expected public-client fields.
	assert.Equal(t, "public", fake.registerBody["clientType"])
	assert.Contains(t, fake.registerBody["grantTypes"], "authorization_code")
	assert.Equal(t, "https://org.awsapps.com/start", fake.registerBody["issuerUrl"])
	redirects, _ := fake.registerBody["redirectUris"].([]any)
	require.Len(t, redirects, 1)
	assert.Equal(t, redirectURI, redirects[0])

	// Authorize URL carries PKCE + state + our redirect_uri.
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "test-client-id", q.Get("client_id"))
	assert.Equal(t, redirectURI, q.Get("redirect_uri"))
	assert.Equal(t, state, q.Get("state"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"))

	// A pending login is tracked for the returned state.
	m.mu.Lock()
	p := m.pending[state]
	m.mu.Unlock()
	require.NotNil(t, p)

	// The challenge must be the S256 of the stored verifier.
	sum := sha256.Sum256([]byte(p.codeVerifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), q.Get("code_challenge"))
}

func TestStartLoginRejectsBadStartURL(t *testing.T) {
	m := newLoginManager(&http.Client{})
	_, _, err := m.startLogin(context.Background(), "", "us-east-1", "", "http://x/cb")
	assert.Error(t, err)
	_, _, err = m.startLogin(context.Background(), "ftp://nope", "us-east-1", "", "http://x/cb")
	assert.Error(t, err)
}

func TestCompleteLoginExchangesCode(t *testing.T) {
	fake := newFakeOIDC(t)
	m := newTestLoginManager(t, fake.srv.URL)

	_, state, err := m.startLogin(context.Background(),
		"https://org.awsapps.com/start", "us-east-1", "team", "http://localhost:27890/oauth/callback")
	require.NoError(t, err)

	acct, err := m.completeLogin(context.Background(), state, "auth-code-123")
	require.NoError(t, err)
	assert.Equal(t, "test-access", acct.AccessToken)
	assert.Equal(t, "test-refresh", acct.RefreshToken)
	assert.Equal(t, "IdC", acct.AuthMethod)
	assert.Equal(t, "team", acct.Label)
	assert.NotEmpty(t, acct.ID)
	assert.NotEmpty(t, acct.ExpiresAt)

	// Token exchange used authorization_code with the stored verifier.
	assert.Equal(t, "authorization_code", fake.tokenBody["grantType"])
	assert.Equal(t, "auth-code-123", fake.tokenBody["code"])
	assert.NotEmpty(t, fake.tokenBody["codeVerifier"])

	// State is single-use: a second completion fails.
	_, err = m.completeLogin(context.Background(), state, "auth-code-123")
	assert.Error(t, err)
}

func TestCompleteLoginUnknownState(t *testing.T) {
	m := newLoginManager(&http.Client{})
	_, err := m.completeLogin(context.Background(), "nope", "code")
	assert.Error(t, err)
}

func TestCompleteLoginMissingCode(t *testing.T) {
	fake := newFakeOIDC(t)
	m := newTestLoginManager(t, fake.srv.URL)
	_, state, err := m.startLogin(context.Background(),
		"https://org.awsapps.com/start", "us-east-1", "", "http://localhost/cb")
	require.NoError(t, err)
	_, err = m.completeLogin(context.Background(), state, "")
	assert.Error(t, err)
}

func TestLoginGCDropsExpired(t *testing.T) {
	m := newLoginManager(&http.Client{})
	m.mu.Lock()
	m.pending["old"] = &pendingLogin{createdAt: time.Now().Add(-2 * loginTTL)}
	m.pending["fresh"] = &pendingLogin{createdAt: time.Now()}
	m.gcLocked()
	_, oldOK := m.pending["old"]
	_, freshOK := m.pending["fresh"]
	m.mu.Unlock()
	assert.False(t, oldOK, "expired pending login should be dropped")
	assert.True(t, freshOK)
}

func TestOIDCBase(t *testing.T) {
	assert.Equal(t, "https://oidc.us-east-1.amazonaws.com", oidcBase("us-east-1"))
	assert.Equal(t, "https://oidc.eu-central-1.amazonaws.com", oidcBase("eu-central-1"))
	assert.Equal(t, "https://oidc.us-east-1.amazonaws.com", oidcBase(""), "empty region defaults to us-east-1")
}

func TestHTMLEscape(t *testing.T) {
	assert.Equal(t, "&lt;script&gt;", htmlEscape("<script>"))
	assert.Equal(t, "a&amp;b", htmlEscape("a&b"))
	assert.False(t, strings.Contains(htmlEscape(`"'`), `"`))
}

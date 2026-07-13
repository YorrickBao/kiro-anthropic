package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// AWS IAM Identity Center (IdC) sign-in via the OAuth 2.0 authorization code
// grant with PKCE, driven from the loopback-only admin page.
//
// Flow (all upstream calls go to https://oidc.<region>.amazonaws.com and are
// routed through the proxy-aware HTTP client):
//
//  1. /api/login/start  -> RegisterClient (public client, authorization_code +
//     refresh_token grants). We mint a PKCE verifier/challenge and a state, then
//     return an authorize URL for the user to open in their browser.
//  2. user approves in the browser; AWS redirects back to /oauth/callback on
//     this same admin origin with ?code&state.
//  3. /oauth/callback   -> CreateToken (authorization_code) exchanges the code
//     for tokens, resolves the profileArn, and persists a StoredAccount.
//
// The redirect_uri is derived from the admin request's own Host, which the
// adminHostOnly middleware guarantees is a loopback name. AWS reliably accepts
// loopback redirect URIs (RFC 8252); remote deployments reach the loopback
// admin port through an SSH tunnel, so the browser's localhost and the server's
// loopback are the same endpoint.
// ---------------------------------------------------------------------------

// kiroOIDCScopes are the CodeWhisperer scopes Kiro registers its client with.
var kiroOIDCScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
	"codewhisperer:transformations",
	"codewhisperer:taskassist",
}

// loginTTL is how long a started login may stay pending before it is discarded.
const loginTTL = 10 * time.Minute

// pendingLogin holds the transient state between /api/login/start and the
// browser redirect to /oauth/callback.
type pendingLogin struct {
	clientID     string
	clientSecret string
	clientSecExp int64
	codeVerifier string
	region       string
	startURL     string
	redirectURI  string
	label        string
	provider     string
	createdAt    time.Time
}

// loginManager tracks pending logins keyed by state and performs the OIDC calls.
type loginManager struct {
	client *http.Client

	mu      sync.Mutex
	pending map[string]*pendingLogin
}

func newLoginManager(client *http.Client) *loginManager {
	return &loginManager{client: client, pending: map[string]*pendingLogin{}}
}

// oidcBase returns the SSO-OIDC endpoint base for a region.
func oidcBase(region string) string {
	if region == "" {
		region = "us-east-1"
	}
	return fmt.Sprintf("https://oidc.%s.amazonaws.com", region)
}

// gc drops pending logins older than loginTTL. Caller holds m.mu.
func (m *loginManager) gcLocked() {
	cutoff := time.Now().Add(-loginTTL)
	for k, v := range m.pending {
		if v.createdAt.Before(cutoff) {
			delete(m.pending, k)
		}
	}
}

// --- wire types --------------------------------------------------------------

type registerClientResp struct {
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt"`
	AuthorizationEndpoint string `json:"authorizationEndpoint"`
	TokenEndpoint         string `json:"tokenEndpoint"`
}

type createTokenResp struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"`
	TokenType    string `json:"tokenType"`
}

// postJSON posts a JSON body to url and decodes a JSON response into out. On a
// non-2xx status it returns an error containing the (truncated) body.
func (m *loginManager) postJSON(ctx context.Context, url string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Mimic the Kiro IDE's SSO-OIDC client (AWS SDK for JavaScript): the real
	// IDE reaches oidc.*.amazonaws.com via the SDK, which stamps aws-sdk-js UA
	// and amz-sdk-* headers on every call. Sending a bare request here would
	// stick out next to those, so we reuse the same header set.
	applyKiroHeaders(req, "", "")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s: %s", url, resp.Status, readSnippet(resp.Body))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// startLogin registers a client and returns the authorize URL and state.
func (m *loginManager) startLogin(ctx context.Context, startURL, region, label, redirectURI string) (authorizeURL, state string, err error) {
	region = strings.TrimSpace(region)
	if region == "" {
		region = "us-east-1"
	}
	startURL = strings.TrimSpace(startURL)
	if startURL == "" {
		return "", "", fmt.Errorf("start_url is required for IdC sign-in")
	}
	if !strings.HasPrefix(startURL, "https://") {
		return "", "", fmt.Errorf("start_url must begin with https://")
	}

	base := oidcBase(region)
	var reg registerClientResp
	err = m.postJSON(ctx, base+"/client/register", map[string]any{
		"clientName":   "kiro-anthropic",
		"clientType":   "public",
		"scopes":       kiroOIDCScopes,
		"grantTypes":   []string{"authorization_code", "refresh_token"},
		"redirectUris": []string{redirectURI},
		"issuerUrl":    startURL,
	}, &reg)
	if err != nil {
		return "", "", fmt.Errorf("register client: %w", err)
	}
	if reg.ClientID == "" {
		return "", "", fmt.Errorf("register client: empty clientId in response")
	}

	// PKCE: verifier is a high-entropy random string; challenge is its SHA-256,
	// base64url without padding (S256).
	verifier, err := randomURLSafe(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state = uuid.NewString()

	m.mu.Lock()
	m.gcLocked()
	m.pending[state] = &pendingLogin{
		clientID:     reg.ClientID,
		clientSecret: reg.ClientSecret,
		clientSecExp: reg.ClientSecretExpiresAt,
		codeVerifier: verifier,
		region:       region,
		startURL:     startURL,
		redirectURI:  redirectURI,
		label:        strings.TrimSpace(label),
		provider:     "Enterprise",
		createdAt:    time.Now(),
	}
	m.mu.Unlock()

	// Build the authorize URL. Prefer the endpoint the service advertised;
	// fall back to the conventional /authorize path.
	authEndpoint := reg.AuthorizationEndpoint
	if authEndpoint == "" {
		authEndpoint = base + "/authorize"
	}
	u, err := url.Parse(authEndpoint)
	if err != nil {
		return "", "", fmt.Errorf("parse authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", reg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// AWS SSO-OIDC's authorize endpoint expects the "scopes" (plural) parameter
	// as a comma-separated list, matching Kiro's own client behaviour.
	q.Set("scopes", strings.Join(kiroOIDCScopes, ","))
	u.RawQuery = q.Encode()
	return u.String(), state, nil
}

// completeLogin exchanges the authorization code for tokens and returns a
// StoredAccount ready to persist. It consumes the pending login for state.
func (m *loginManager) completeLogin(ctx context.Context, state, code string) (*StoredAccount, error) {
	m.mu.Lock()
	m.gcLocked()
	p := m.pending[state]
	if p != nil {
		delete(m.pending, state)
	}
	m.mu.Unlock()

	if p == nil {
		return nil, fmt.Errorf("unknown or expired login state")
	}
	if code == "" {
		return nil, fmt.Errorf("missing authorization code")
	}

	base := oidcBase(p.region)
	var tok createTokenResp
	err := m.postJSON(ctx, base+"/token", map[string]any{
		"clientId":     p.clientID,
		"clientSecret": p.clientSecret,
		"grantType":    "authorization_code",
		"code":         code,
		"redirectUri":  p.redirectURI,
		"codeVerifier": p.codeVerifier,
	}, &tok)
	if err != nil {
		return nil, fmt.Errorf("exchange code for token: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned an empty accessToken")
	}

	now := time.Now().UTC()
	acct := &StoredAccount{
		ID:                    uuid.NewString(),
		Label:                 p.label,
		Provider:              p.provider,
		AuthMethod:            "IdC",
		Region:                p.region,
		StartURL:              p.startURL,
		ClientID:              p.clientID,
		ClientSecret:          p.clientSecret,
		ClientSecretExpiresAt: p.clientSecExp,
		AccessToken:           tok.AccessToken,
		RefreshToken:          tok.RefreshToken,
		CreatedAt:             now.Format(time.RFC3339),
	}
	if tok.ExpiresIn > 0 {
		acct.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second).Format("2006-01-02T15:04:05.000Z")
	}

	// Resolve the profileArn best-effort; failure is non-fatal (it can be
	// resolved later when the account is put into service).
	if arn, err := resolveProfileArn(ctx, m.client, p.region, tok.AccessToken); err == nil {
		acct.ProfileArn = arn
	}
	acct.Email = fetchAccountEmail(ctx, m.client, p.region, acct.ProfileArn, tok.AccessToken)
	return acct, nil
}

// importLocalCredentials builds a StoredAccount from an existing Kiro auth
// cache: the token file plus its client registration companion. It resolves the
// profileArn best-effort. The returned
// account has a fresh id; the caller is responsible for dedup and persistence.
func importLocalCredentials(ctx context.Context, client *http.Client, tokenFile string) (*StoredAccount, error) {
	tok, _, err := loadToken(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("token file has no refreshToken (nothing to import)")
	}

	clientID, clientSecret := findClientRegistration(tokenFile, tok.ClientIDHash)

	region := tok.Region
	if region == "" {
		region = "us-east-1"
	}
	acct := &StoredAccount{
		ID:           uuid.NewString(),
		Label:        "imported",
		Provider:     tok.Provider,
		AuthMethod:   tok.AuthMethod,
		Region:       region,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		ProfileArn:   tok.ProfileArn,
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if acct.ProfileArn == "" && tok.AccessToken != "" {
		if arn, err := resolveProfileArn(ctx, client, region, tok.AccessToken); err == nil {
			acct.ProfileArn = arn
		}
	}
	if tok.AccessToken != "" {
		acct.Email = fetchAccountEmail(ctx, client, region, acct.ProfileArn, tok.AccessToken)
	}
	return acct, nil
}

// importLocalIntoStore imports the local Kiro credentials into the account
// store, deduping against existing accounts (refreshing in place on a match).
// Returns 1 if an account was added, 0 if it was already present (refreshed) or
// there was nothing to import. An error means the local cache was unreadable.
func importLocalIntoStore(ctx context.Context, store *AccountStore, client *http.Client, tokenFile string) (int, error) {
	acct, err := importLocalCredentials(ctx, client, tokenFile)
	if err != nil {
		return 0, err
	}
	if id, ok := store.FindDuplicate(*acct); ok {
		return 0, store.ReplaceCredentials(id, acct)
	}
	if err := store.Add(acct); err != nil {
		return 0, err
	}
	return 1, nil
}

// findClientRegistration locates the clientId/clientSecret for an imported
// token. It first tries the <clientIdHash>.json companion next to the token
// file, then falls back to scanning the directory for any registration file.
func findClientRegistration(tokenFile, clientIDHash string) (clientID, clientSecret string) {
	dir := filepath.Dir(tokenFile)
	if clientIDHash != "" {
		if reg, err := readClientRegistration(filepath.Join(dir, clientIDHash+".json")); err == nil {
			return reg.ClientID, reg.ClientSecret
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	base := filepath.Base(tokenFile)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == base {
			continue
		}
		if reg, err := readClientRegistration(filepath.Join(dir, e.Name())); err == nil &&
			reg.ClientID != "" && reg.ClientSecret != "" {
			return reg.ClientID, reg.ClientSecret
		}
	}
	return "", ""
}

// readClientRegistration reads a <clientIdHash>.json client registration file.
func readClientRegistration(path string) (clientRegistration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return clientRegistration{}, err
	}
	var reg clientRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		return clientRegistration{}, err
	}
	return reg, nil
}

// refreshAccountToken performs an SSO-OIDC refresh_token grant for a stored IdC
// account, using the account's own clientId/clientSecret/refreshToken. It
// returns the new access token, refresh token (may be unchanged) and RFC3339
// expiry. It operates on a StoredAccount and does not touch Kiro's on-disk cache.
func refreshAccountToken(ctx context.Context, client *http.Client, a StoredAccount) (accessToken, refreshToken, expiresAt string, err error) {
	if a.RefreshToken == "" {
		return "", "", "", fmt.Errorf("account has no refreshToken")
	}
	if a.ClientID == "" || a.ClientSecret == "" {
		return "", "", "", fmt.Errorf("account has no client registration")
	}
	region := a.Region
	if region == "" {
		region = "us-east-1"
	}

	// CreateToken is an unauthenticated public API; AnonymousCredentials skips
	// SigV4 signing. Region drives the oidc.<region>.amazonaws.com endpoint and
	// HTTPClient reuses our proxy-aware client.
	oidc := ssooidc.New(ssooidc.Options{
		Region:      region,
		HTTPClient:  client,
		Credentials: aws.AnonymousCredentials{},
	})
	out, err := oidc.CreateToken(ctx, &ssooidc.CreateTokenInput{
		ClientId:     aws.String(a.ClientID),
		ClientSecret: aws.String(a.ClientSecret),
		GrantType:    aws.String("refresh_token"),
		RefreshToken: aws.String(a.RefreshToken),
	})
	if err != nil {
		return "", "", "", fmt.Errorf("call SSO-OIDC CreateToken: %w", err)
	}
	if out.AccessToken == nil || *out.AccessToken == "" {
		return "", "", "", fmt.Errorf("CreateToken returned an empty accessToken")
	}
	accessToken = *out.AccessToken
	if out.RefreshToken != nil {
		refreshToken = *out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).
			UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return accessToken, refreshToken, expiresAt, nil
}

// randomURLSafe returns n random bytes encoded as base64url without padding.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// fetchAccountEmail resolves the account email via getUsageLimits (userInfo.email)
// using the given bearer token. It is best-effort: an empty string is returned
// on any failure, since the email is only cosmetic (shown in the admin list).
func fetchAccountEmail(ctx context.Context, client *http.Client, region, arn, accessToken string) string {
	if region == "" {
		region = "us-east-1"
	}
	endpoint := fmt.Sprintf("https://management.%s.kiro.dev/getUsageLimits", region)
	q := url.Values{
		"origin":          {"AI_EDITOR"},
		"resourceType":    {"AGENTIC_REQUEST"},
		"isEmailRequired": {"true"},
	}
	if arn != "" {
		q.Set("profileArn", arn)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	applyKiroHeaders(req, accessToken, "")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var out struct {
		UserInfo struct {
			Email string `json:"email"`
		} `json:"userInfo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return out.UserInfo.Email
}

// resolveProfileArn calls ListAvailableProfiles on the management endpoint with
// the given bearer token and returns the first profile's arn.
func resolveProfileArn(ctx context.Context, client *http.Client, region, accessToken string) (string, error) {
	if region == "" {
		region = "us-east-1"
	}
	endpoint := fmt.Sprintf("https://management.%s.kiro.dev/", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableProfiles")
	applyKiroHeaders(req, accessToken, "")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, readSnippet(resp.Body))
	}
	var out struct {
		Profiles []struct {
			Arn string `json:"arn"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Profiles) == 0 || out.Profiles[0].Arn == "" {
		return "", fmt.Errorf("no profiles returned")
	}
	return out.Profiles[0].Arn, nil
}

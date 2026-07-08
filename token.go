package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
)

// tokenRefreshBuffer is how long before real expiry we proactively refresh.
const tokenRefreshBuffer = 5 * time.Minute

// Fixed profile ARNs used by Kiro for the free / social sign-in tiers. Enterprise
// (IdC) accounts do not have a fixed ARN and must resolve it via the backend.
const socialSignInProfileArn = "arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK"

var fixedProfileArns = map[string]string{
	"BuilderId": "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX",
	"Github":    socialSignInProfileArn,
	"Google":    socialSignInProfileArn,
}

// Token mirrors the on-disk kiro-auth-token.json. Unknown fields are preserved
// separately (see rawToken) so we never clobber data Kiro relies on.
type Token struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	ClientIDHash string `json:"clientIdHash"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider"`
	Region       string `json:"region"`
	ProfileArn   string `json:"profileArn"` // present on some social tokens
}

// expiry parses the ExpiresAt field into a time. Zero time means unknown.
func (t Token) expiry() time.Time {
	if t.ExpiresAt == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
	} {
		if ts, err := time.Parse(layout, t.ExpiresAt); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func (t Token) valid(at time.Time) bool {
	exp := t.expiry()
	if exp.IsZero() {
		// If we cannot parse expiry, treat the token as usable and let the
		// server surface any 403 from the backend.
		return t.AccessToken != ""
	}
	return exp.After(at)
}

// clientRegistration mirrors the <clientIdHash>.json cache file.
type clientRegistration struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// TokenStore owns the token lifecycle: loading, refreshing and profileArn
// resolution. It is safe for concurrent use.
type TokenStore struct {
	cfg    *Config
	client *http.Client

	mu          sync.Mutex
	tok         Token
	resolvedArn string
	arnResolved bool
}

// NewTokenStore loads the token from disk and prepares the store.
func NewTokenStore(cfg *Config, client *http.Client) (*TokenStore, error) {
	if cfg.TokenFile == "" {
		return nil, fmt.Errorf("token file path is empty (could not determine home dir)")
	}
	tok, _, err := loadToken(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", cfg.TokenFile, err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("no accessToken in %s (is Kiro signed in?)", cfg.TokenFile)
	}
	return &TokenStore{cfg: cfg, client: client, tok: tok}, nil
}

// loadToken reads the token file into both the typed struct and a raw map (so
// unknown keys can be written back verbatim).
func loadToken(path string) (Token, map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Token{}, nil, err
	}
	var tok Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return Token{}, nil, fmt.Errorf("parse token json: %w", err)
	}
	raw := map[string]json.RawMessage{}
	_ = json.Unmarshal(data, &raw)
	return tok, raw, nil
}

// snapshot returns a copy of the current in-memory token.
func (s *TokenStore) snapshot() Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tok
}

// region returns the effective region (config override > token > us-east-1).
func (s *TokenStore) region() string {
	if s.cfg.Region != "" {
		return s.cfg.Region
	}
	s.mu.Lock()
	r := s.tok.Region
	s.mu.Unlock()
	if r != "" {
		return r
	}
	return "us-east-1"
}

// AccessToken returns a currently-valid bearer token, refreshing if needed.
func (s *TokenStore) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(tokenRefreshBuffer)
	if s.tok.valid(cutoff) {
		return s.tok.AccessToken, nil
	}

	// The token looks stale. Kiro (the IDE) may have refreshed it already, so
	// re-read the file before doing our own refresh.
	if fresh, _, err := loadToken(s.cfg.TokenFile); err == nil && fresh.valid(cutoff) {
		s.tok = fresh
		return s.tok.AccessToken, nil
	}

	if err := s.refreshLocked(ctx); err != nil {
		// If refresh fails but we still hold a token that isn't hard-expired,
		// use it and let the backend decide.
		if s.tok.valid(time.Now()) {
			return s.tok.AccessToken, nil
		}
		return "", err
	}
	return s.tok.AccessToken, nil
}

// refreshLocked performs an SSO-OIDC CreateToken refresh_token grant (via the
// aws-sdk-go-v2 ssooidc client, routed through our proxy-aware HTTP client) and
// persists the result. Caller must hold s.mu.
func (s *TokenStore) refreshLocked(ctx context.Context) error {
	if s.tok.RefreshToken == "" {
		return fmt.Errorf("token expired and no refreshToken available; sign in with Kiro again")
	}

	reg, err := s.loadClientRegistration()
	if err != nil {
		return fmt.Errorf("load client registration: %w", err)
	}

	// CreateToken is an unauthenticated public API; AnonymousCredentials skips
	// SigV4 signing. Region drives the default oidc.<region>.amazonaws.com
	// endpoint, and HTTPClient reuses our proxy-aware client.
	oidc := ssooidc.New(ssooidc.Options{
		Region:      s.region(),
		HTTPClient:  s.client,
		Credentials: aws.AnonymousCredentials{},
	})

	out, err := oidc.CreateToken(ctx, &ssooidc.CreateTokenInput{
		ClientId:     aws.String(reg.ClientID),
		ClientSecret: aws.String(reg.ClientSecret),
		GrantType:    aws.String("refresh_token"),
		RefreshToken: aws.String(s.tok.RefreshToken),
	})
	if err != nil {
		return fmt.Errorf("call SSO-OIDC CreateToken: %w", err)
	}
	if out.AccessToken == nil || *out.AccessToken == "" {
		return fmt.Errorf("CreateToken returned an empty accessToken")
	}

	s.tok.AccessToken = *out.AccessToken
	if out.RefreshToken != nil && *out.RefreshToken != "" {
		s.tok.RefreshToken = *out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		s.tok.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).
			UTC().Format("2006-01-02T15:04:05.000Z")
	}

	if err := s.writeTokenLocked(); err != nil {
		// Non-fatal: we still have a good in-memory token for this run.
		fmt.Fprintf(os.Stderr, "warning: refreshed token but could not persist it: %v\n", err)
	}
	return nil
}

// writeTokenLocked writes the updated token back to disk, preserving any
// unknown fields present in the original file. Caller must hold s.mu.
func (s *TokenStore) writeTokenLocked() error {
	_, raw, err := loadToken(s.cfg.TokenFile)
	if err != nil || raw == nil {
		raw = map[string]json.RawMessage{}
	}
	set := func(key, val string) {
		b, _ := json.Marshal(val)
		raw[key] = b
	}
	set("accessToken", s.tok.AccessToken)
	set("refreshToken", s.tok.RefreshToken)
	set("expiresAt", s.tok.ExpiresAt)

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}

	// Write atomically to avoid a half-written file if Kiro reads concurrently.
	dir := filepath.Dir(s.cfg.TokenFile)
	tmp, err := os.CreateTemp(dir, ".kiro-auth-token-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.cfg.TokenFile)
}

// loadClientRegistration reads the <clientIdHash>.json companion file.
func (s *TokenStore) loadClientRegistration() (clientRegistration, error) {
	if s.tok.ClientIDHash == "" {
		return clientRegistration{}, fmt.Errorf("token has no clientIdHash")
	}
	dir := filepath.Dir(s.cfg.TokenFile)
	path := filepath.Join(dir, s.tok.ClientIDHash+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return clientRegistration{}, err
	}
	var reg clientRegistration
	if err := json.Unmarshal(data, &reg); err != nil {
		return clientRegistration{}, err
	}
	if reg.ClientID == "" || reg.ClientSecret == "" {
		return clientRegistration{}, fmt.Errorf("client registration missing clientId/clientSecret")
	}
	return reg, nil
}

// ProfileArn resolves the CodeWhisperer profileArn required by the runtime.
// Resolution order: explicit config > social token's own arn > fixed arn for
// the provider > backend ListAvailableProfiles. The result is cached.
func (s *TokenStore) ProfileArn(ctx context.Context) (string, error) {
	if s.cfg.ProfileArn != "" {
		return s.cfg.ProfileArn, nil
	}

	s.mu.Lock()
	if s.arnResolved {
		arn := s.resolvedArn
		s.mu.Unlock()
		return arn, nil
	}
	provider := s.tok.Provider
	authMethod := s.tok.AuthMethod
	tokenArn := s.tok.ProfileArn
	s.mu.Unlock()

	// Social sign-in tokens may carry their own arn.
	if authMethod == "social" && tokenArn != "" {
		s.cacheArn(tokenArn)
		return tokenArn, nil
	}
	if arn, ok := fixedProfileArns[provider]; ok && arn != "" {
		s.cacheArn(arn)
		return arn, nil
	}

	// Enterprise / IdC: ask the control plane.
	arn, err := s.listFirstProfileArn(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn via ListAvailableProfiles: %w", err)
	}
	s.cacheArn(arn)
	return arn, nil
}

func (s *TokenStore) cacheArn(arn string) {
	s.mu.Lock()
	s.resolvedArn = arn
	s.arnResolved = true
	s.mu.Unlock()
}

// listFirstProfileArn calls AmazonCodeWhispererService.ListAvailableProfiles on
// the management endpoint and returns the first profile's arn.
func (s *TokenStore) listFirstProfileArn(ctx context.Context) (string, error) {
	token, err := s.AccessToken(ctx)
	if err != nil {
		return "", err
	}
	region := s.region()
	endpoint := fmt.Sprintf("https://management.%s.kiro.dev/", region)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, readSnippet(resp.Body))
	}

	var out struct {
		Profiles []struct {
			Arn         string `json:"arn"`
			ProfileName string `json:"profileName"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode profiles: %w", err)
	}
	if len(out.Profiles) == 0 || out.Profiles[0].Arn == "" {
		return "", fmt.Errorf("no profiles returned")
	}
	return out.Profiles[0].Arn, nil
}

// ForceRefresh unconditionally performs a token refresh. Used to recover from a
// 401/403 from the backend even if our local expiry check thought the token was
// still valid.
func (s *TokenStore) ForceRefresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Prefer a fresher on-disk token (Kiro may have rotated it) before we spend
	// our refresh token.
	if fresh, _, err := loadToken(s.cfg.TokenFile); err == nil &&
		fresh.AccessToken != "" && fresh.AccessToken != s.tok.AccessToken {
		s.tok = fresh
		if s.tok.valid(time.Now().Add(tokenRefreshBuffer)) {
			return nil
		}
	}
	return s.refreshLocked(ctx)
}

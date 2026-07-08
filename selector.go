package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Per-request credentials and multi-account selection.
//
// KiroClient.Send is written against the kiroCredentials interface so a request
// can be served either by the single primary account loaded from Kiro's cache
// (storeCreds, the original behaviour) or by one of the self-managed accounts in
// the multi-account store (accountCreds), chosen round-robin by accountSelector.
// ---------------------------------------------------------------------------

// kiroCredentials supplies the bearer token, region and profileArn for one
// GenerateAssistantResponse attempt, and can refresh its token on a 401/403.
type kiroCredentials interface {
	// accessToken returns a currently-valid bearer token, refreshing if needed.
	accessToken(ctx context.Context) (string, error)
	// apiRegion is the region hosting the Kiro runtime/management endpoints.
	apiRegion() string
	// profileArn resolves the CodeWhisperer profileArn for the request.
	profileArn(ctx context.Context) (string, error)
	// refresh unconditionally refreshes the token (used to recover from 401/403).
	refresh(ctx context.Context) error
}

// storeCreds adapts the primary TokenStore to kiroCredentials. This is the
// original single-account path; behaviour is unchanged.
type storeCreds struct {
	store *TokenStore
}

func (c storeCreds) accessToken(ctx context.Context) (string, error) { return c.store.AccessToken(ctx) }
func (c storeCreds) apiRegion() string                               { return c.store.apiRegion() }
func (c storeCreds) profileArn(ctx context.Context) (string, error)  { return c.store.ProfileArn(ctx) }
func (c storeCreds) refresh(ctx context.Context) error               { return c.store.ForceRefresh(ctx) }

// accountCreds serves a request from one stored (self-managed) account. Token
// refresh uses the account's own client registration and is written back to the
// account store; it never touches Kiro's on-disk cache.
type accountCreds struct {
	store  *AccountStore
	client *http.Client
	id     string
	acct   StoredAccount
}

func (c *accountCreds) apiRegion() string {
	if c.acct.Region != "" {
		return c.acct.Region
	}
	return "us-east-1"
}

func (c *accountCreds) profileArn(ctx context.Context) (string, error) {
	if c.acct.ProfileArn != "" {
		return c.acct.ProfileArn, nil
	}
	// Resolve best-effort onto this request's account copy (accounts normally
	// already carry a ProfileArn, so this is a rare fallback path).
	arn, err := resolveProfileArn(ctx, c.client, c.apiRegion(), c.acct.AccessToken)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn for account %s: %w", c.id, err)
	}
	c.acct.ProfileArn = arn
	return arn, nil
}

func (c *accountCreds) accessToken(ctx context.Context) (string, error) {
	// Refresh proactively when the token is missing or near expiry. The
	// background refresher normally keeps it fresh; this covers the gap.
	exp := c.acct.expiry()
	needs := c.acct.AccessToken == "" ||
		(!exp.IsZero() && time.Now().Add(tokenRefreshBuffer).After(exp))
	if needs {
		if err := c.refresh(ctx); err != nil {
			// If we still hold a token, use it and let the backend decide.
			if c.acct.AccessToken != "" {
				return c.acct.AccessToken, nil
			}
			return "", err
		}
	}
	return c.acct.AccessToken, nil
}

func (c *accountCreds) refresh(ctx context.Context) error {
	access, refresh, expiresAt, err := refreshAccountToken(ctx, c.client, c.acct)
	if err != nil {
		return err
	}
	c.acct.AccessToken = access
	if refresh != "" {
		c.acct.RefreshToken = refresh
	}
	c.acct.ExpiresAt = expiresAt
	// Persist best-effort; an in-memory token still serves this request.
	_ = c.store.UpdateTokens(c.id, c.acct.AccessToken, c.acct.RefreshToken, c.acct.ExpiresAt)
	return nil
}

// ---------------------------------------------------------------------------
// Account selector: round-robin over the store with a lightweight cooldown.
// ---------------------------------------------------------------------------

// accountCooldown is how long a failing account is skipped before it is retried.
const accountCooldown = 60 * time.Second

// accountSelector picks accounts round-robin, skipping those in cooldown.
type accountSelector struct {
	store  *AccountStore
	client *http.Client

	mu       sync.Mutex
	index    int
	cooldown map[string]time.Time // account id -> time the cooldown ends
}

func newAccountSelector(store *AccountStore, client *http.Client) *accountSelector {
	return &accountSelector{store: store, client: client, cooldown: map[string]time.Time{}}
}

// pick returns credentials for the next eligible account, skipping ids in tried
// and accounts currently in cooldown. When every remaining account is in
// cooldown it falls back to the one whose cooldown ends soonest. ok is false
// only when there are no accounts left to try (store empty or all tried).
func (s *accountSelector) pick(tried map[string]bool) (*accountCreds, bool) {
	list := s.store.List()
	if len(list) == 0 {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	// Round-robin scan for an account that is neither tried nor cooling down.
	var soonest *StoredAccount
	var soonestAt time.Time
	n := len(list)
	for i := 0; i < n; i++ {
		a := list[(s.index+i)%n]
		if tried[a.ID] {
			continue
		}
		if until, ok := s.cooldown[a.ID]; ok && now.Before(until) {
			// Track the candidate whose cooldown ends soonest as a fallback.
			if soonest == nil || until.Before(soonestAt) {
				ac := a
				soonest, soonestAt = &ac, until
			}
			continue
		}
		s.index = (s.index + i + 1) % n
		return s.credsFor(a), true
	}

	// Everything untried is cooling down: use the soonest-recovering account.
	if soonest != nil {
		return s.credsFor(*soonest), true
	}
	return nil, false
}

// credsFor builds accountCreds for the given account snapshot.
func (s *accountSelector) credsFor(a StoredAccount) *accountCreds {
	return &accountCreds{store: s.store, client: s.client, id: a.ID, acct: a}
}

// recordSuccess clears any cooldown on the account.
func (s *accountSelector) recordSuccess(id string) {
	s.mu.Lock()
	delete(s.cooldown, id)
	s.mu.Unlock()
}

// recordFailure puts the account into cooldown.
func (s *accountSelector) recordFailure(id string) {
	s.mu.Lock()
	s.cooldown[id] = time.Now().Add(accountCooldown)
	s.mu.Unlock()
}

// isAccountFailure reports whether err is an upstream failure that warrants
// trying a different account (auth, quota/throttle, upstream 5xx, or a
// transport error). A 400 that is not a thinking-signature issue is a problem
// with the request itself and should surface immediately without burning other
// accounts.
func isAccountFailure(err error) bool {
	if err == nil {
		return false
	}
	he, ok := err.(*kiroHTTPError)
	if !ok {
		// Transport/stream error (e.g. connection reset): try another account.
		return true
	}
	switch {
	case he.Status == http.StatusUnauthorized, he.Status == http.StatusForbidden:
		return true
	case he.Status == http.StatusPaymentRequired, he.Status == http.StatusTooManyRequests:
		return true
	case he.Status == 423: // Locked (account suspended)
		return true
	case he.Status >= 500:
		return true
	default:
		return false
	}
}

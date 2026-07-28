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
// KiroClient.Send and the control-plane calls are written against the
// kiroCredentials interface so they act as a specific account (accountCreds),
// chosen round-robin by accountSelector.
// ---------------------------------------------------------------------------

// kiroCredentials supplies the bearer token, region and profileArn for one
// Kiro call, and can refresh its token on a 401/403.
type kiroCredentials interface {
	// accessToken returns a currently-valid bearer token, refreshing if needed.
	accessToken(ctx context.Context) (string, error)
	// apiRegion is the region hosting the Kiro runtime/management endpoints.
	apiRegion() string
	// profileArn resolves the CodeWhisperer profileArn for the request.
	profileArn(ctx context.Context) (string, error)
	// machineID returns a stable per-account fingerprint for the User-Agent,
	// so pooled accounts look like distinct Kiro IDE installs rather than one
	// host running many sessions. Empty means "no fingerprint".
	machineID() string
	// refresh unconditionally refreshes the token (used to recover from 401/403).
	refresh(ctx context.Context) error
}

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

// machineID returns a stable fingerprint for this account, embedded in the
// User-Agent so each pooled account looks like its own Kiro IDE install.
func (c *accountCreds) machineID() string { return machineIDFor(c.id) }

func (c *accountCreds) profileArn(ctx context.Context) (string, error) {
	if c.acct.ProfileArn != "" {
		return c.acct.ProfileArn, nil
	}
	// Resolve best-effort onto this request's account copy (accounts normally
	// already carry a ProfileArn, so this is a rare fallback path). Use a fresh
	// token via accessToken() rather than the possibly-stale stored one, so an
	// account with an empty profileArn but a valid refreshToken self-heals
	// instead of failing ListAvailableProfiles with a 401. (accessToken() does
	// not call profileArn(), so there is no recursion.)
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn for account %s: %w", c.id, err)
	}
	arn, err := resolveProfileArn(ctx, c.client, c.apiRegion(), tok)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn for account %s: %w", c.id, err)
	}
	c.acct.ProfileArn = arn
	// Persist the resolved profileArn so it survives restarts and shows up on
	// the admin page. Best-effort: a write failure does not affect this request.
	_ = c.store.UpdateIdentity(c.id, arn, "", "")
	// While we have a valid token, also resolve email and userId best-effort and
	// persist them for the same reason (and to backfill userId onto older records
	// that predate the field). fetchAccountIdentity is non-fatal on failure.
	if c.acct.Email == "" || c.acct.UserID == "" {
		ident := fetchAccountIdentity(ctx, c.client, c.apiRegion(), arn, tok)
		if ident.Email != "" {
			c.acct.Email = ident.Email
		}
		if ident.UserID != "" {
			c.acct.UserID = ident.UserID
		}
		if ident.Email != "" || ident.UserID != "" {
			_ = c.store.UpdateIdentity(c.id, "", ident.Email, ident.UserID)
		}
	}
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
	// Route through the store so concurrent refreshes of this account (across
	// requests and the background refresher) collapse into one CreateToken call.
	fresh, err := c.store.RefreshToken(ctx, c.client, c.id)
	if err != nil {
		return err
	}
	c.acct = fresh
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
	s.pruneCooldownLocked(list)

	// Round-robin scan for an account that is neither tried nor cooling down.
	var soonest *StoredAccount
	var soonestAt time.Time
	n := len(list)
	for i := 0; i < n; i++ {
		a := list[(s.index+i)%n]
		if tried[a.ID] {
			continue
		}
		if !accountUsable(a) {
			continue // dead account: no profileArn and no way to obtain one
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

// accountUsable reports whether an account can serve a request at all: it must
// not be administratively parked (Disabled), and it must either already carry a
// profileArn, or hold a full client registration plus refresh token so
// accessToken()/profileArn() can refresh and resolve one. An account with
// neither (e.g. an import whose profileArn lookup failed and that lacks
// credentials to recover) is dead weight and is skipped during selection.
// Disabled accounts are still stored, refreshed and shown on the admin page;
// this gate only excludes them from selection.
func accountUsable(a StoredAccount) bool {
	if a.Disabled {
		return false
	}
	if a.ProfileArn != "" {
		return true
	}
	return a.RefreshToken != "" && a.ClientID != "" && a.ClientSecret != ""
}

// pruneCooldownLocked drops cooldown entries for accounts no longer in the
// store, so removed accounts do not leak entries. Caller must hold s.mu.
func (s *accountSelector) pruneCooldownLocked(list []StoredAccount) {
	if len(s.cooldown) == 0 {
		return
	}
	live := make(map[string]bool, len(list))
	for _, a := range list {
		live[a.ID] = true
	}
	for id := range s.cooldown {
		if !live[id] {
			delete(s.cooldown, id)
		}
	}
}

// peekAny returns credentials for one eligible account for a control-plane call
// (model schema lookup) that is not tied to a specific request. Unlike pick it
// does NOT advance the round-robin cursor, so schema lookups do not perturb
// request dispatch fairness (which matters especially with few accounts). It
// skips unusable accounts and prefers one not in cooldown, falling back to the
// first usable account. ok is false when no usable account exists.
func (s *accountSelector) peekAny() (*accountCreds, bool) {
	list := s.store.List()
	if len(list) == 0 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var firstUsable *StoredAccount
	for i := range list {
		a := list[i]
		if !accountUsable(a) {
			continue
		}
		if firstUsable == nil {
			firstUsable = &list[i]
		}
		if until, ok := s.cooldown[a.ID]; ok && now.Before(until) {
			continue
		}
		return s.credsFor(a), true
	}
	// All usable accounts are cooling down: any of them serves a (cached)
	// schema lookup. If none are usable at all, report no account.
	if firstUsable != nil {
		return s.credsFor(*firstUsable), true
	}
	return nil, false
}

// byID returns credentials for one specific account by id, regardless of its
// usability/cooldown state — used by per-account control-plane calls that the
// admin explicitly targets (e.g. "show models for THIS account's card"). ok is
// false when the id is unknown.
func (s *accountSelector) byID(id string) (*accountCreds, bool) {
	for _, a := range s.store.List() {
		if a.ID == id {
			return s.credsFor(a), true
		}
	}
	return nil, false
}

// listAll returns credentials for every stored account, for per-account
// control-plane calls (e.g. rendering usage for each account on the admin page).
func (s *accountSelector) listAll() []*accountCreds {
	list := s.store.List()
	out := make([]*accountCreds, 0, len(list))
	for _, a := range list {
		out = append(out, s.credsFor(a))
	}
	return out
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

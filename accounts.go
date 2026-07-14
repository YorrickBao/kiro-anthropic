package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

// ---------------------------------------------------------------------------
// Multi-account credential store.
//
// This is the persistence layer for the account pool, the sole source of
// accounts used to serve requests and control-plane calls. Accounts arrive via
// startup import of the local Kiro cache, admin-page sign-in, or the import
// button (see login.go).
//
// Credentials are long-lived secrets (refreshToken, clientSecret). The file is
// written 0600 in a 0700 directory; callers should treat it as sensitive.
// ---------------------------------------------------------------------------

// StoredAccount is one signed-in account persisted to disk. Field names are the
// on-disk JSON schema; keep them stable.
type StoredAccount struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	Email      string `json:"email,omitempty"`      // account email, resolved from getUsageLimits
	Provider   string `json:"provider,omitempty"`   // e.g. "Enterprise", "BuilderId"
	AuthMethod string `json:"authMethod,omitempty"` // e.g. "IdC"
	Region     string `json:"region,omitempty"`
	StartURL   string `json:"startUrl,omitempty"`

	// OIDC client registration (from RegisterClient).
	ClientID              string `json:"clientId,omitempty"`
	ClientSecret          string `json:"clientSecret,omitempty"`
	ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt,omitempty"` // unix seconds

	// Tokens (from CreateToken).
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"` // RFC3339

	ProfileArn string `json:"profileArn,omitempty"`
	// UserID is the IAM Identity Center user identity from getUsageLimits
	// (userInfo.userId, format "d-<directory>.<uuid>"). Unlike profileArn (shared
	// by every user in an IdC organization) and email (which an admin can change),
	// it is globally unique per person and stable across email changes, so it is
	// the primary dedup key. Resolved from the same call as Email, so both are
	// present or both absent; older records predating this field carry only Email
	// until a lazy resolve backfills it.
	UserID    string `json:"userId,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`

	// Disabled omits the account from the round-robin pool: it is still stored,
	// refreshed and shown on the admin page with usage, but never selected to
	// serve requests. Defaults to false (opt-out), preserving legacy behaviour.
	Disabled bool `json:"disabled,omitempty"`
}

// expiry parses ExpiresAt; zero time means unknown.
func (a StoredAccount) expiry() time.Time {
	if a.ExpiresAt == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z"} {
		if ts, err := time.Parse(layout, a.ExpiresAt); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// AccountStore persists StoredAccounts to a JSON file. Safe for concurrent use.
type AccountStore struct {
	path string

	mu       sync.Mutex
	accounts map[string]*StoredAccount

	// refreshGroup collapses concurrent token refreshes of the same account (by
	// id) into a single SSO-OIDC CreateToken call. AWS rotates refresh tokens on
	// every refresh, so two independent refreshers racing on one account would
	// each spend the (now stale) refresh token and one would fail with
	// invalid_grant, or fork the token chain. Both the request path
	// (accountCreds) and the background refresher route through RefreshToken.
	refreshGroup singleflight.Group
}

// accountsFile mirrors the on-disk layout.
type accountsFile struct {
	Accounts []*StoredAccount `json:"accounts"`
}

// NewAccountStore loads accounts from path (an empty/missing file is fine).
func NewAccountStore(path string) (*AccountStore, error) {
	if path == "" {
		return nil, fmt.Errorf("accounts file path is empty (could not determine home dir)")
	}
	s := &AccountStore{path: path, accounts: map[string]*StoredAccount{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the file into memory. A non-existent file is treated as empty.
func (s *AccountStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var f accountsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range f.Accounts {
		if a != nil && a.ID != "" {
			s.accounts[a.ID] = a
		}
	}
	return nil
}

// saveLocked writes the current accounts to disk atomically. Caller holds s.mu.
func (s *AccountStore) saveLocked() error {
	list := make([]*StoredAccount, 0, len(s.accounts))
	for _, a := range s.accounts {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })

	out, err := json.MarshalIndent(accountsFile{Accounts: list}, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create accounts dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// Add inserts or replaces an account and persists the store.
func (s *AccountStore) Add(a *StoredAccount) error {
	if a == nil || a.ID == "" {
		return fmt.Errorf("account must have an id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[a.ID] = a
	return s.saveLocked()
}

// ReplaceCredentials refreshes the credential and identity fields of an existing
// account from a newly obtained one (same AWS account, new sign-in/import),
// preserving the existing id, label and creation time. Returns an error if id
// is unknown.
func (s *AccountStore) ReplaceCredentials(id string, fresh *StoredAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account %s not found", id)
	}
	applyCredentialsLocked(a, fresh)
	return s.saveLocked()
}

// applyCredentialsLocked overwrites the credential and identity fields of a with
// those from fresh (same AWS account, new sign-in/import/migration), preserving
// a's id, label and creation time. Empty profileArn/email in fresh are ignored
// so a partial refresh does not erase previously resolved identity. Caller holds
// s.mu.
func applyCredentialsLocked(a, fresh *StoredAccount) {
	a.Provider = fresh.Provider
	a.AuthMethod = fresh.AuthMethod
	a.Region = fresh.Region
	a.StartURL = fresh.StartURL
	a.ClientID = fresh.ClientID
	a.ClientSecret = fresh.ClientSecret
	a.ClientSecretExpiresAt = fresh.ClientSecretExpiresAt
	a.AccessToken = fresh.AccessToken
	a.RefreshToken = fresh.RefreshToken
	a.ExpiresAt = fresh.ExpiresAt
	if fresh.ProfileArn != "" {
		a.ProfileArn = fresh.ProfileArn
	}
	if fresh.Email != "" {
		a.Email = fresh.Email
	}
	if fresh.UserID != "" {
		a.UserID = fresh.UserID
	}
}

// ImportResult summarizes a bulk import (see ImportAccounts).
type ImportResult struct {
	Added    int
	Replaced int
}

// ImportAccounts merges externally exported accounts (see the admin export
// bundle) into the store for server-to-server migration. Each incoming account
// is deduped by stable identity via findDuplicateLocked: a match has its
// credentials replaced in place (preserving the existing id/label/createdAt),
// while a new account is inserted, minting a fresh id when the export carried
// none or one that collides with an unrelated account. Entries without any
// usable credential are skipped. The store is persisted once, after all merges.
func (s *AccountStore) ImportAccounts(incoming []*StoredAccount) (ImportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var res ImportResult
	for _, in := range incoming {
		if in == nil || (in.RefreshToken == "" && in.AccessToken == "") {
			continue
		}
		if id, ok := s.findDuplicateLocked(*in); ok {
			applyCredentialsLocked(s.accounts[id], in)
			res.Replaced++
			continue
		}
		na := *in
		if na.ID == "" {
			na.ID = uuid.NewString()
		}
		if _, exists := s.accounts[na.ID]; exists {
			// An unrelated account already owns this id (identity did not match);
			// mint a new one rather than clobber it.
			na.ID = uuid.NewString()
		}
		if na.CreatedAt == "" {
			na.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		s.accounts[na.ID] = &na
		res.Added++
	}
	if res.Added == 0 && res.Replaced == 0 {
		return res, nil
	}
	if err := s.saveLocked(); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

// SetDisabled toggles whether an account participates in the round-robin pool
// and persists the store. A disabled account remains stored, refreshed and
// shown on the admin page; it is only excluded from selection. Returns an error
// if the id is unknown.
func (s *AccountStore) SetDisabled(id string, disabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account %s not found", id)
	}
	a.Disabled = disabled
	return s.saveLocked()
}

// UpdateLabel sets the label (note) of an existing account and persists the
// store. Returns an error if the id is unknown.
func (s *AccountStore) UpdateLabel(id, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account %s not found", id)
	}
	a.Label = label
	return s.saveLocked()
}

// UpdateIdentity persists the profileArn, email and/or userId of an existing
// account. Empty values are ignored so a partial resolution does not erase
// previously stored identity. This is used by the lazy resolver in selector.go
// to write back identity resolved at request time, so it survives restarts and
// is visible on the admin page. It also backfills userId onto older records that
// predate the field. Returns an error if the id is unknown.
func (s *AccountStore) UpdateIdentity(id, profileArn, email, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account %s not found", id)
	}
	if profileArn != "" {
		a.ProfileArn = profileArn
	}
	if email != "" {
		a.Email = email
	}
	if userID != "" {
		a.UserID = userID
	}
	return s.saveLocked()
}

// UpdateTokens updates the token fields of an existing account and persists the
// store. It is a no-op error if the id is unknown. Only token/expiry fields are
// touched; registration and identity fields are left intact.
func (s *AccountStore) UpdateTokens(id, accessToken, refreshToken, expiresAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return fmt.Errorf("account %s not found", id)
	}
	a.AccessToken = accessToken
	if refreshToken != "" {
		a.RefreshToken = refreshToken
	}
	a.ExpiresAt = expiresAt
	return s.saveLocked()
}

// RefreshToken refreshes the account's SSO-OIDC token exactly once even under
// concurrent callers, returning the refreshed account snapshot. Concurrent
// calls for the same id share a single CreateToken call and all receive the
// same result; this prevents racing refreshers from each spending the rotated
// refresh token. The fresh tokens are persisted best-effort before returning,
// so the returned snapshot is usable even if the write fails.
func (s *AccountStore) RefreshToken(ctx context.Context, client *http.Client, id string) (StoredAccount, error) {
	v, err, _ := s.refreshGroup.Do(id, func() (any, error) {
		cur, ok := s.Get(id)
		if !ok {
			return StoredAccount{}, fmt.Errorf("account %s not found", id)
		}
		access, refresh, expiresAt, err := refreshAccountToken(ctx, client, cur)
		if err != nil {
			return StoredAccount{}, err
		}
		// Persist best-effort; the returned snapshot still carries the new token.
		_ = s.UpdateTokens(id, access, refresh, expiresAt)
		cur.AccessToken = access
		if refresh != "" {
			cur.RefreshToken = refresh
		}
		cur.ExpiresAt = expiresAt
		return cur, nil
	})
	if err != nil {
		return StoredAccount{}, err
	}
	return v.(StoredAccount), nil
}

// RefreshIdentity re-resolves the account's profileArn, email and userId from
// the Kiro management endpoint and persists them. Unlike the lazy resolver in
// selector.go, which only fills fields that are empty, this forces a fresh
// lookup and overwrites the stored identity, so an admin can correct stale
// identity from the admin page. The management endpoint rejects a stale token,
// so the token is refreshed first when missing or near expiry. Values that come
// back empty are left untouched (see UpdateIdentity), so a partial lookup never
// erases existing identity. Returns the refreshed account snapshot.
func (s *AccountStore) RefreshIdentity(ctx context.Context, client *http.Client, id string) (StoredAccount, error) {
	cur, ok := s.Get(id)
	if !ok {
		return StoredAccount{}, fmt.Errorf("account %s not found", id)
	}
	// Ensure a usable token: the ListAvailableProfiles / getUsageLimits calls
	// 401 on an expired one. Fall back to the stored token only if a refresh
	// fails but we still hold something to try.
	exp := cur.expiry()
	if cur.AccessToken == "" || (!exp.IsZero() && time.Now().Add(tokenRefreshBuffer).After(exp)) {
		if fresh, err := s.RefreshToken(ctx, client, id); err == nil {
			cur = fresh
		} else if cur.AccessToken == "" {
			return StoredAccount{}, err
		}
	}
	region := cur.Region
	if region == "" {
		region = "us-east-1"
	}
	arn, err := resolveProfileArn(ctx, client, region, cur.AccessToken)
	if err != nil {
		return StoredAccount{}, fmt.Errorf("resolve identity for account %s: %w", id, err)
	}
	ident := fetchAccountIdentity(ctx, client, region, arn, cur.AccessToken)
	if err := s.UpdateIdentity(id, arn, ident.Email, ident.UserID); err != nil {
		return StoredAccount{}, err
	}
	updated, _ := s.Get(id)
	return updated, nil
}

// Get returns a copy of the account with the given id.
func (s *AccountStore) Get(id string) (StoredAccount, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return StoredAccount{}, false
	}
	return *a, true
}

// Remove deletes an account and persists the store. Removing a missing id is a
// no-op that still succeeds.
func (s *AccountStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[id]; !ok {
		return nil
	}
	delete(s.accounts, id)
	return s.saveLocked()
}

// FindDuplicate returns the id of an existing account that represents the same
// AWS account as the candidate, if any. It matches on stable identity rather
// than credentials, so the same account added via different paths (sign-in vs
// import) is recognised as one: a login and an import of the same account get
// distinct clientId/refreshToken pairs, but share profileArn and email.
//
// Matching rules, in priority order:
//
//  0. userId: the IAM Identity Center user identity (StoredAccount.UserID). When
//     both sides have a non-empty userId it is decisive — same userId → duplicate,
//     different userId → never a duplicate. Unlike email it survives an admin
//     changing the account's email, so re-signing-in after an email change is
//     recognised as the same account instead of leaving an orphan. Older records
//     predating the field lack userId and fall through to the email rule until a
//     lazy resolve backfills it.
//  1. email: when both sides have a non-empty email (and userId did not decide),
//     it is the decisive key. Same email → duplicate; different email → never a
//     duplicate, even if profileArn matches (multiple users in the same IdC
//     organization share a profileArn but are distinct people).
//  2. profileArn: when email is unavailable on at least one side, a matching
//     non-empty profileArn is used as a fallback.
//  3. clientId backfill: the candidate carries identity (userId/profileArn/email)
//     that the stored account is missing, but they share the same clientId.
//     This lets a sign-in that resolved identity backfill an earlier import
//     that could not, replacing it in place instead of creating a duplicate.
//  4. clientId only: neither side has identity, but they share the same
//     clientId — a same-machine re-import whose identity lookup failed both times.
//
// clientId is never used to match a candidate that has NO identity against a
// stored account that DOES: two different AWS accounts signed in via the same
// IdC start URL can share an OIDC client registration, so a bare clientId match
// would wrongly merge distinct users.
func (s *AccountStore) FindDuplicate(candidate StoredAccount) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findDuplicateLocked(candidate)
}

// findDuplicateLocked is FindDuplicate's body; caller holds s.mu.
func (s *AccountStore) findDuplicateLocked(candidate StoredAccount) (string, bool) {
	candHasIdentity := candidate.UserID != "" || candidate.ProfileArn != "" || candidate.Email != ""
	for _, a := range s.accounts {
		// Rule 0: userId is the definitive IdC user identity. When both sides have
		// it, it decides outright — and a mismatch means provably different users,
		// so no lower rule may override it (email/profileArn cannot resurrect a
		// match the userId ruled out).
		if candidate.UserID != "" && a.UserID != "" {
			if candidate.UserID == a.UserID {
				return a.ID, true
			}
			continue
		}
		// Rule 1: when both emails are known, email is the decisive identity.
		if candidate.Email != "" && a.Email != "" {
			if candidate.Email == a.Email {
				return a.ID, true
			}
			// Different emails → provably different users; skip this account.
			// (profileArn must NOT override this, even if it matches.)
			continue
		}
		// Rule 2: profileArn fallback — only when email is unavailable on BOTH
		// sides. If one side has email, we cannot confirm they are the same user
		// just from profileArn (multiple users share a profile in IdC).
		if candidate.Email == "" && a.Email == "" &&
			candidate.ProfileArn != "" && a.ProfileArn == candidate.ProfileArn {
			return a.ID, true
		}
		// Rule 3: backfill — candidate has identity, stored has none, same clientId.
		if candHasIdentity && a.ProfileArn == "" && a.Email == "" &&
			candidate.ClientID != "" && a.ClientID == candidate.ClientID {
			return a.ID, true
		}
		// Rule 4: neither side has identity; same machine re-import.
		if !candHasIdentity && a.ProfileArn == "" && a.Email == "" &&
			candidate.ClientID != "" && a.ClientID == candidate.ClientID {
			return a.ID, true
		}
	}
	return "", false
}

// List returns copies of all stored accounts, ordered by creation time.
func (s *AccountStore) List() []StoredAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StoredAccount, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// view renders an account as a redacted map for the admin API: secrets are
// masked, and an expiry state is computed.
func (a StoredAccount) view() map[string]any {
	m := map[string]any{
		"id":           a.ID,
		"label":        a.Label,
		"email":        a.Email,
		"provider":     a.Provider,
		"auth_method":  a.AuthMethod,
		"region":       a.Region,
		"start_url":    a.StartURL,
		"profile_arn":  a.ProfileArn,
		"user_id":      a.UserID,
		"created_at":   a.CreatedAt,
		"expires_at":   a.ExpiresAt,
		"disabled":     a.Disabled,
		"access_token": masked(a.AccessToken),
		"has_refresh":  a.RefreshToken != "",
	}
	exp := a.expiry()
	switch {
	case exp.IsZero():
		m["expiry_state"] = "unknown"
	default:
		d := time.Until(exp).Round(time.Second)
		state := "valid"
		switch {
		case d <= 0:
			state = "expired"
		case d < tokenRefreshBuffer:
			state = "expiring soon"
		}
		m["expiry_state"] = state
		m["expires_in_seconds"] = int(d.Seconds())
	}
	return m
}

// defaultAccountsFile returns the default path for the accounts store.
func defaultAccountsFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kiro-anthropic", "accounts.json")
}

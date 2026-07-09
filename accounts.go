package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Multi-account credential store.
//
// This is the persistence layer for accounts signed in through the admin page
// (see login.go). It is deliberately independent of TokenStore, which owns the
// single active account loaded from Kiro's own cache: this store is where
// self-managed logins live until a later step wires them into request serving.
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
	CreatedAt  string `json:"createdAt,omitempty"`
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
// Precedence: profileArn (most reliable for enterprise) > email > the legacy
// clientId+refreshToken pair (used only when no identity fields are available).
func (s *AccountStore) FindDuplicate(candidate StoredAccount) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		switch {
		case candidate.ProfileArn != "" && a.ProfileArn == candidate.ProfileArn:
			return a.ID, true
		case candidate.Email != "" && a.Email == candidate.Email:
			return a.ID, true
		case candidate.ProfileArn == "" && candidate.Email == "" &&
			candidate.RefreshToken != "" &&
			a.RefreshToken == candidate.RefreshToken && a.ClientID == candidate.ClientID:
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
		"created_at":   a.CreatedAt,
		"expires_at":   a.ExpiresAt,
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

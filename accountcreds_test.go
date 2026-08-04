package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeControlPlane stands in for both oidc.<region>.amazonaws.com (/token) and
// management.<region>.kiro.dev (ListAvailableProfiles). It counts calls and can
// reject stale tokens on the profiles endpoint, so tests can observe the
// refresh-then-resolve self-heal path.
type fakeControlPlane struct {
	srv           *httptest.Server
	tokenHits     int32
	profileHits   int32
	profileArn    string // arn to return from ListAvailableProfiles
	freshAccess   string // access token minted by /token
	acceptToken   string // if set, ListAvailableProfiles 401s unless bearer matches this
	profilesEmpty bool   // return an empty profiles list
}

func newFakeControlPlane(t *testing.T) *fakeControlPlane {
	t.Helper()
	f := &fakeControlPlane{profileArn: "arn:aws:codewhisperer:us-east-1:1:profile/abc", freshAccess: "fresh-access"}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.tokenHits, 1)
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken": f.freshAccess, "refreshToken": "rotated-refresh",
			"tokenType": "Bearer", "expiresIn": 3600,
		})
	})
	// Management endpoint is POST "/" dispatched by X-Amz-Target.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != "AmazonCodeWhispererService.ListAvailableProfiles" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&f.profileHits, 1)
		if f.acceptToken != "" && r.Header.Get("Authorization") != "Bearer "+f.acceptToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"expired"}`))
			return
		}
		if f.profilesEmpty {
			writeJSON(w, http.StatusOK, map[string]any{"profiles": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"profiles": []map[string]any{{"arn": f.profileArn}},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeControlPlane) credsClient(t *testing.T) *http.Client {
	t.Helper()
	target, err := url.Parse(f.srv.URL)
	require.NoError(t, err)
	return &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}
}

// credsFor builds an accountCreds backed by a real store so refresh writes back.
func credsForAccount(t *testing.T, client *http.Client, a StoredAccount) (*accountCreds, *AccountStore) {
	t.Helper()
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	if a.ID == "" {
		a.ID = "acct"
	}
	if a.CreatedAt == "" {
		a.CreatedAt = "1"
	}
	require.NoError(t, store.Add(&a))
	rt, ok := store.Runtime(a.ID)
	require.True(t, ok)
	return &accountCreds{
		store: store, client: client, id: a.ID,
		revision: rt.Revision, credential: rt.Credential, acct: rt.Account,
	}, store
}

// ---- accountUsable ---------------------------------------------------------

func TestAccountUsable(t *testing.T) {
	cases := []struct {
		name string
		a    StoredAccount
		want bool
	}{
		{"profileArn only", StoredAccount{ProfileArn: "arn:x"}, true},
		{"full creds, no arn", StoredAccount{RefreshToken: "r", ClientID: "c", ClientSecret: "s"}, true},
		{"arn plus creds", StoredAccount{ProfileArn: "arn:x", RefreshToken: "r", ClientID: "c", ClientSecret: "s"}, true},
		{"empty", StoredAccount{}, false},
		{"refresh only", StoredAccount{RefreshToken: "r"}, false},
		{"missing clientSecret", StoredAccount{RefreshToken: "r", ClientID: "c"}, false},
		{"missing clientId", StoredAccount{RefreshToken: "r", ClientSecret: "s"}, false},
		{"creds but no refresh", StoredAccount{ClientID: "c", ClientSecret: "s"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, accountUsable(c.a))
		})
	}
}

// ---- accountCreds.profileArn self-heal ------------------------------------

func TestProfileArnReturnsStoredWithoutNetwork(t *testing.T) {
	f := newFakeControlPlane(t)
	c, _ := credsForAccount(t, f.credsClient(t), StoredAccount{
		ProfileArn: "arn:stored", AccessToken: "tok",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	arn, err := c.profileArn(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "arn:stored", arn)
	assert.Equal(t, int32(0), atomic.LoadInt32(&f.profileHits), "no lookup when arn is stored")
	assert.Equal(t, int32(0), atomic.LoadInt32(&f.tokenHits), "no refresh when arn is stored")
}

func TestProfileArnResolvesWithValidToken(t *testing.T) {
	f := newFakeControlPlane(t)
	f.acceptToken = "valid" // profiles endpoint accepts this token as-is
	c, store := credsForAccount(t, f.credsClient(t), StoredAccount{
		AccessToken: "valid", ClientID: "c", ClientSecret: "s", RefreshToken: "r",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), // fresh -> no refresh
	})
	arn, err := c.profileArn(context.Background())
	require.NoError(t, err)
	assert.Equal(t, f.profileArn, arn)
	assert.Equal(t, int32(0), atomic.LoadInt32(&f.tokenHits), "valid token needs no refresh")
	assert.Equal(t, int32(1), atomic.LoadInt32(&f.profileHits))
	// Resolved arn is cached back onto the in-memory account copy.
	assert.Equal(t, f.profileArn, c.acct.ProfileArn)
	_ = store
}

func TestProfileArnSelfHealsWithExpiredToken(t *testing.T) {
	f := newFakeControlPlane(t)
	// The profiles endpoint only accepts the freshly-minted token, so resolve
	// succeeds only if profileArn() refreshes the expired stored token first.
	f.acceptToken = f.freshAccess
	c, store := credsForAccount(t, f.credsClient(t), StoredAccount{
		AccessToken: "stale", ClientID: "c", ClientSecret: "s", RefreshToken: "r",
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), // expired -> must refresh
	})
	arn, err := c.profileArn(context.Background())
	require.NoError(t, err)
	assert.Equal(t, f.profileArn, arn)
	assert.Equal(t, int32(1), atomic.LoadInt32(&f.tokenHits), "expired token triggers exactly one refresh")
	assert.GreaterOrEqual(t, atomic.LoadInt32(&f.profileHits), int32(1))
	// New token persisted by the refresh path.
	got, _ := store.Get(c.id)
	assert.Equal(t, f.freshAccess, got.AccessToken)
}

func TestProfileArnResolveErrorSurfaces(t *testing.T) {
	f := newFakeControlPlane(t)
	f.profilesEmpty = true // ListAvailableProfiles returns no profiles -> error
	c, _ := credsForAccount(t, f.credsClient(t), StoredAccount{
		AccessToken: "valid", ClientID: "c", ClientSecret: "s", RefreshToken: "r",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	_, err := c.profileArn(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve profileArn")
	assert.Empty(t, c.acct.ProfileArn, "nothing cached on failure")
}

func TestProfileArnRefreshFailureSurfaces(t *testing.T) {
	// No refresh token / registration and no stored arn: accessToken() cannot
	// mint a token, so profileArn() fails without hitting the profiles endpoint.
	f := newFakeControlPlane(t)
	c, _ := credsForAccount(t, f.credsClient(t), StoredAccount{
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	_, err := c.profileArn(context.Background())
	require.Error(t, err)
	assert.Equal(t, int32(0), atomic.LoadInt32(&f.profileHits), "no profiles call without a token")
}

// ---- selector filtering with cooldown interplay ---------------------------

func TestSelectorUnusableSkippedEvenWhenUsableCoolingDown(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{ID: "dead", CreatedAt: "0"})) // unusable
	require.NoError(t, store.Add(&StoredAccount{
		ID: "good", ProfileArn: "arn:x", ClientID: "c", ClientSecret: "s", RefreshToken: "r",
		OverageEnabled: true, CreatedAt: "1",
	}))
	s := newAccountSelector(store, &http.Client{})

	// Put the only usable account into cooldown.
	first := s.pickFor(map[string]bool{}, "")
	require.NotNil(t, first.lease)
	s.recordFailure(first.lease)
	// pick must still choose "good" (soonest-recovering usable) and never "dead".
	picked := s.pickFor(map[string]bool{}, "")
	require.NotNil(t, picked.lease)
	assert.Equal(t, "good", picked.lease.creds.id)
}

func TestPeekAnyPrefersUsableNotInCooldown(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{ID: "dead", CreatedAt: "0"})) // unusable, listed first
	require.NoError(t, store.Add(&StoredAccount{ID: "good", ProfileArn: "arn:x", CreatedAt: "1"}))
	s := newAccountSelector(store, &http.Client{})

	creds, ok := s.peekAny()
	require.True(t, ok)
	assert.Equal(t, "good", creds.id, "peekAny skips the unusable head")
}

func TestPeekAnyAllUsableCoolingDownStillReturnsUsable(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{ID: "dead", CreatedAt: "0"}))
	require.NoError(t, store.Add(&StoredAccount{
		ID: "good", ProfileArn: "arn:x", OverageEnabled: true, CreatedAt: "1",
	}))
	s := newAccountSelector(store, &http.Client{})
	picked := s.pickFor(map[string]bool{}, "")
	require.NotNil(t, picked.lease)
	s.recordFailure(picked.lease)

	creds, ok := s.peekAny()
	require.True(t, ok, "a cooling-down usable account still serves a cached schema lookup")
	assert.Equal(t, "good", creds.id)
}

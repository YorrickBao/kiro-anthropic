package main

import (
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSelector(t *testing.T, ids ...string) *accountSelector {
	t.Helper()
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	for i, id := range ids {
		// CreatedAt controls List() ordering, hence round-robin order.
		require.NoError(t, store.Add(&StoredAccount{
			ID: id, ClientID: "c", ClientSecret: "s", RefreshToken: "r",
			Region: "us-east-1", CreatedAt: string(rune('0' + i)),
		}))
	}
	return newAccountSelector(store, &http.Client{})
}

func TestSelectorRoundRobin(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c")
	var got []string
	for i := 0; i < 6; i++ {
		creds, ok := s.pick(map[string]bool{})
		require.True(t, ok)
		got = append(got, creds.id)
		s.recordSuccess(creds.id)
	}
	assert.Equal(t, []string{"a", "b", "c", "a", "b", "c"}, got)
}

func TestSelectorPeekAnyDoesNotAdvance(t *testing.T) {
	s := newTestSelector(t, "a", "b")

	// Repeated peeks must not move the cursor...
	for i := 0; i < 5; i++ {
		creds, ok := s.peekAny()
		require.True(t, ok)
		assert.Equal(t, "a", creds.id, "peek is stable and non-advancing")
	}
	// ...so the next pick still returns the head, then rotation proceeds.
	c1, _ := s.pick(map[string]bool{})
	c2, _ := s.pick(map[string]bool{})
	assert.Equal(t, "a", c1.id)
	assert.Equal(t, "b", c2.id)
}

func TestSelectorPeekAnyEmpty(t *testing.T) {
	s := newTestSelector(t)
	_, ok := s.peekAny()
	assert.False(t, ok)
}

func TestSelectorEmpty(t *testing.T) {
	s := newTestSelector(t)
	_, ok := s.pick(map[string]bool{})
	assert.False(t, ok)
}

func TestSelectorSkipsTried(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c")
	tried := map[string]bool{"a": true, "b": true}
	creds, ok := s.pick(tried)
	require.True(t, ok)
	assert.Equal(t, "c", creds.id)

	tried["c"] = true
	_, ok = s.pick(tried)
	assert.False(t, ok, "all tried -> nothing left")
}

func TestSelectorCooldownSkip(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	// Fail "a": it should be skipped while "b" is served.
	first, ok := s.pick(map[string]bool{})
	require.True(t, ok)
	s.recordFailure(first.id)

	// Next pick (fresh request, nothing tried) should avoid the cooled-down one.
	next, ok := s.pick(map[string]bool{})
	require.True(t, ok)
	assert.NotEqual(t, first.id, next.id, "cooled-down account is skipped")
}

func TestSelectorAllCooldownFallsBackToSoonest(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	// Put both in cooldown, "b" recovering sooner.
	s.mu.Lock()
	s.cooldown["a"] = time.Now().Add(10 * time.Minute)
	s.cooldown["b"] = time.Now().Add(1 * time.Minute)
	s.mu.Unlock()

	creds, ok := s.pick(map[string]bool{})
	require.True(t, ok)
	assert.Equal(t, "b", creds.id, "falls back to the soonest-recovering account")
}

func TestSelectorSuccessClearsCooldown(t *testing.T) {
	s := newTestSelector(t, "a")
	s.recordFailure("a")
	s.mu.Lock()
	_, cooling := s.cooldown["a"]
	s.mu.Unlock()
	require.True(t, cooling)

	s.recordSuccess("a")
	s.mu.Lock()
	_, stillCooling := s.cooldown["a"]
	s.mu.Unlock()
	assert.False(t, stillCooling)
}

func TestSelectorCooldownPrunedForRemovedAccount(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	s.recordFailure("a")
	s.recordFailure("b")
	s.mu.Lock()
	require.Len(t, s.cooldown, 2)
	s.mu.Unlock()

	// Remove "a" from the store; the next pick should prune its cooldown entry.
	require.NoError(t, s.store.Remove("a"))
	_, _ = s.pick(map[string]bool{})

	s.mu.Lock()
	_, aStale := s.cooldown["a"]
	s.mu.Unlock()
	assert.False(t, aStale, "cooldown entry for a removed account must be pruned")
}

func TestSelectorSkipsUnusableAccount(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	// "dead": no profileArn and no credentials to obtain one -> unusable.
	require.NoError(t, store.Add(&StoredAccount{ID: "dead", CreatedAt: "0"}))
	// "good": has a refresh token + client registration -> usable.
	require.NoError(t, store.Add(&StoredAccount{
		ID: "good", ClientID: "c", ClientSecret: "s", RefreshToken: "r", CreatedAt: "1",
	}))
	s := newAccountSelector(store, &http.Client{})

	for i := 0; i < 5; i++ {
		creds, ok := s.pick(map[string]bool{})
		require.True(t, ok)
		assert.Equal(t, "good", creds.id, "unusable account must never be selected")
		s.recordSuccess(creds.id)
	}

	// An account carrying only a profileArn (no creds) is still usable.
	require.NoError(t, store.Add(&StoredAccount{ID: "arn-only", ProfileArn: "arn:x", CreatedAt: "2"}))
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		creds, _ := s.pick(map[string]bool{})
		seen[creds.id] = true
		s.recordSuccess(creds.id)
	}
	assert.True(t, seen["arn-only"], "profileArn-only account is usable")
	assert.False(t, seen["dead"], "dead account still skipped")
}

func TestSelectorAllUnusableReturnsFalse(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{ID: "d1", CreatedAt: "0"}))
	require.NoError(t, store.Add(&StoredAccount{ID: "d2", RefreshToken: "r", CreatedAt: "1"})) // missing clientId/secret
	s := newAccountSelector(store, &http.Client{})

	_, ok := s.pick(map[string]bool{})
	assert.False(t, ok, "no usable account -> pick fails (maps to 503)")
	_, ok = s.peekAny()
	assert.False(t, ok, "no usable account -> peekAny fails")
}

func TestSelectorSkipsDisabledAccount(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	// "off" is a fully-credentialed account but parked out of the pool.
	require.NoError(t, store.Add(&StoredAccount{
		ID: "off", ClientID: "c", ClientSecret: "s", RefreshToken: "r", Disabled: true, CreatedAt: "0",
	}))
	// "on" is identical but participates.
	require.NoError(t, store.Add(&StoredAccount{
		ID: "on", ClientID: "c", ClientSecret: "s", RefreshToken: "r", CreatedAt: "1",
	}))
	s := newAccountSelector(store, &http.Client{})

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		creds, ok := s.pick(map[string]bool{})
		require.True(t, ok)
		seen[creds.id] = true
		s.recordSuccess(creds.id)
	}
	assert.True(t, seen["on"], "enabled account must be selected")
	assert.False(t, seen["off"], "disabled account must never be selected")

	// accountUsable reflects the same gate.
	assert.False(t, accountUsable(StoredAccount{ProfileArn: "arn:x", Disabled: true}))
	assert.True(t, accountUsable(StoredAccount{ProfileArn: "arn:x"}))

	// Re-enabling brings it back into rotation.
	require.NoError(t, store.SetDisabled("off", false))
	seen = map[string]bool{}
	for i := 0; i < 6; i++ {
		creds, _ := s.pick(map[string]bool{})
		seen[creds.id] = true
		s.recordSuccess(creds.id)
	}
	assert.True(t, seen["off"], "re-enabled account rejoins the pool")
}

func TestSelectorAllDisabledReturnsFalse(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "off1", ClientID: "c", ClientSecret: "s", RefreshToken: "r", Disabled: true, CreatedAt: "0",
	}))
	require.NoError(t, store.Add(&StoredAccount{
		ID: "off2", ClientID: "c", ClientSecret: "s", RefreshToken: "r", Disabled: true, CreatedAt: "1",
	}))
	s := newAccountSelector(store, &http.Client{})

	_, ok := s.pick(map[string]bool{})
	assert.False(t, ok, "all-disabled pool -> pick fails (maps to 503)")
	_, ok = s.peekAny()
	assert.False(t, ok, "all-disabled pool -> peekAny fails (no schema source)")
}

func TestSelectorConcurrentPick(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c", "d")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if creds, ok := s.pick(map[string]bool{}); ok {
				s.recordSuccess(creds.id)
			}
		}()
	}
	wg.Wait() // -race verifies no data race on index/cooldown maps.
}

func TestIsAccountFailure(t *testing.T) {
	assert.False(t, isAccountFailure(nil))

	// Transport / non-HTTP error -> failover.
	assert.True(t, isAccountFailure(assertAnError{}))

	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusPaymentRequired, true},
		{http.StatusTooManyRequests, true},
		{423, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusBadRequest, false},
		{http.StatusUnprocessableEntity, false},
	}
	for _, c := range cases {
		got := isAccountFailure(&kiroHTTPError{Status: c.status})
		assert.Equalf(t, c.want, got, "status %d", c.status)
	}
}

// assertAnError is a non-kiroHTTPError error used to exercise the transport path.
type assertAnError struct{}

func (assertAnError) Error() string { return "boom" }

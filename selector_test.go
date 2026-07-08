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

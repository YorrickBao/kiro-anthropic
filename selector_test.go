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

func TestSelectorDepletedSkip(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	// Park "a" as depleted: it should be skipped while "b" is served.
	s.markDepleted("a", time.Now().Add(depletedFallbackTTL))

	for i := 0; i < 4; i++ {
		creds, ok := s.pick(map[string]bool{})
		require.True(t, ok)
		assert.Equal(t, "b", creds.id, "depleted account is skipped")
		s.recordSuccess(creds.id)
	}
}

func TestSelectorDepletedFallbackToSoonest(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	// Both depleted, "b" recovering sooner.
	s.markDepleted("a", time.Now().Add(10*time.Minute))
	s.markDepleted("b", time.Now().Add(time.Minute))

	creds, ok := s.pick(map[string]bool{})
	require.True(t, ok)
	assert.Equal(t, "b", creds.id, "falls back to the soonest-recovering depleted account")
}

func TestSelectorIsDepletedOnlyWhileMarkIsActive(t *testing.T) {
	s := newTestSelector(t, "a")

	s.markDepleted("a", time.Now().Add(time.Minute))
	assert.True(t, s.isDepleted("a"))

	s.mu.Lock()
	s.depleted["a"] = depletedEntry{until: time.Now().Add(-time.Minute)}
	s.mu.Unlock()
	assert.False(t, s.isDepleted("a"), "expired marks must not affect read-only queries")
	assert.False(t, s.isDepleted("missing"))
}

func TestSelectorDepletedExpiresAndReturns(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	// "a" depleted in the past -> already recoverable, selectable again.
	s.markDepleted("a", time.Now().Add(-time.Minute))
	creds, ok := s.pick(map[string]bool{})
	require.True(t, ok)
	assert.Equal(t, "a", creds.id, "an expired depleted mark no longer skips the account")
}

func TestSelectorApplyUsageRemainingPositive(t *testing.T) {
	s := newTestSelector(t, "a")
	s.markDepleted("a", time.Now().Add(depletedFallbackTTL))

	s.applyUsage("a", &kiroUsage{Credit: &kiroCreditUsage{Remaining: 42}}, time.Now(), false)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.False(t, depleted, "credit remaining lifts the depleted mark")
}

func TestSelectorApplyUsageRemainingZeroWithReset(t *testing.T) {
	s := newTestSelector(t, "a")
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	s.applyUsage("a", &kiroUsage{
		Credit:  &kiroCreditUsage{Remaining: 0},
		ResetAt: reset.Format(time.RFC3339),
	}, time.Now(), false)

	s.mu.Lock()
	e, depleted := s.depleted["a"]
	s.mu.Unlock()
	require.True(t, depleted, "exhausted account is parked")
	assert.WithinDuration(t, reset, e.until, time.Second, "parked until its reset_at")
}

func TestSelectorApplyUsageRemainingZeroNoReset(t *testing.T) {
	s := newTestSelector(t, "a")
	before := time.Now()

	s.applyUsage("a", &kiroUsage{Credit: &kiroCreditUsage{Remaining: 0}}, time.Now(), false)

	s.mu.Lock()
	e, depleted := s.depleted["a"]
	s.mu.Unlock()
	require.True(t, depleted, "exhausted account with unknown reset is parked")
	assert.WithinDuration(t, before.Add(depletedFallbackTTL), e.until, time.Second,
		"parked for the fallback TTL when reset_at is unknown")
}

func TestSelectorApplyUsageNilIsNoop(t *testing.T) {
	s := newTestSelector(t, "a")
	s.markDepleted("a", time.Now().Add(depletedFallbackTTL))

	s.applyUsage("a", nil, time.Now(), false)          // unknown usage -> stay conservative
	s.applyUsage("a", &kiroUsage{}, time.Now(), false) // no credit line -> stay conservative

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.True(t, depleted, "nil/credit-less usage leaves the depleted mark untouched")
}

// #1: a request-path fallback deadline must not shorten a precise reset_at that
// a probe/admin refresh already recorded.
func TestMarkDepletedKeepsLaterDeadline(t *testing.T) {
	s := newTestSelector(t, "a")
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	// Probe/admin records the precise reset_at...
	s.applyUsage("a", &kiroUsage{
		Credit:  &kiroCreditUsage{Remaining: 0},
		ResetAt: reset.Format(time.RFC3339),
	}, time.Now(), false)
	// ...then a retry fails and the request path parks with the fallback TTL.
	s.markDepleted("a", time.Now().Add(depletedFallbackTTL))

	s.mu.Lock()
	e, depleted := s.depleted["a"]
	s.mu.Unlock()
	require.True(t, depleted)
	assert.WithinDuration(t, reset, e.until, time.Second, "precise reset_at is not shortened to the fallback")
}

// #4: a stale admin-page usage snapshot (remaining>0) must not lift a mark a
// concurrent request failure just set.
func TestApplyUsageStaleSnapshotDoesNotLiftMark(t *testing.T) {
	s := newTestSelector(t, "a")
	staleFetch := time.Now().Add(-time.Minute) // snapshot taken before the failure
	s.markDepleted("a", time.Now().Add(depletedFallbackTTL))

	// The stale snapshot (remaining>0) arrives after the failure: must not lift.
	s.applyUsage("a", &kiroUsage{Credit: &kiroCreditUsage{Remaining: 42}}, staleFetch, false)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.True(t, depleted, "a stale snapshot must not lift a fresher depleted mark")
}

// #3: the probe path (preciseOnly) refines existing marks but never re-parks an
// account that recovered and served traffic since the snapshot was taken.
func TestApplyUsagePreciseOnlyDoesNotCreate(t *testing.T) {
	s := newTestSelector(t, "a")
	// No existing mark (account recovered); a preciseOnly zero snapshot must not
	// create one.
	s.applyUsage("a", &kiroUsage{Credit: &kiroCreditUsage{Remaining: 0}}, time.Now(), true)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.False(t, depleted, "preciseOnly applyUsage must not create a new depleted mark")
}

// #5: when every account is skipped, the fallback ranks by true recovery time
// (the later of cooldown/depleted), not whichever skip fired first.
func TestSkipUntilRanksByLaterDeadline(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	now := time.Now()
	// "a": cooldown ends soon but depleted for long; "b": depleted sooner overall.
	s.mu.Lock()
	s.cooldown["a"] = now.Add(30 * time.Second)
	s.depleted["a"] = depletedEntry{until: now.Add(10 * time.Minute), basis: now}
	s.depleted["b"] = depletedEntry{until: now.Add(5 * time.Minute), basis: now}
	s.mu.Unlock()

	creds, ok := s.pick(map[string]bool{})
	require.True(t, ok)
	assert.Equal(t, "b", creds.id, "fallback picks the truly sooner-recovering account")
}

func TestRecordSuccessClearsDepleted(t *testing.T) {
	s := newTestSelector(t, "a")
	s.markDepleted("a", time.Now().Add(depletedFallbackTTL))

	s.recordSuccess("a")

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.False(t, depleted, "recordSuccess lifts the depleted mark")
}

func TestIsAccountDepleted(t *testing.T) {
	assert.False(t, isAccountDepleted(nil))
	assert.False(t, isAccountDepleted(assertAnError{})) // transport error -> not depleted

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"402 payment required", &kiroHTTPError{Status: http.StatusPaymentRequired}, true},
		{"423 locked", &kiroHTTPError{Status: http.StatusLocked}, true},
		{"quota event", &kiroHTTPError{Kind: "serviceQuotaExceededError"}, true},
		{"429 throttle", &kiroHTTPError{Status: http.StatusTooManyRequests}, false},
		{"500 server", &kiroHTTPError{Status: http.StatusInternalServerError}, false},
		{"502 gateway", &kiroHTTPError{Status: http.StatusBadGateway}, false},
		{"400 request", &kiroHTTPError{Status: http.StatusBadRequest}, false},
		{"other event kind", &kiroHTTPError{Kind: "internalServerException"}, false},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, isAccountDepleted(c.err), "%s", c.name)
	}
}

func TestOverageRemaining(t *testing.T) {
	cases := []struct {
		name string
		c    *kiroCreditUsage
		want float64
	}{
		{"nil", nil, 0},
		{"no overage configured", &kiroCreditUsage{OverageCap: 0, Used: 5, Limit: 10}, 0},
		{"not yet in overage", &kiroCreditUsage{OverageCap: 100, Used: 5, Limit: 10}, 100},
		{"spending overage", &kiroCreditUsage{OverageCap: 100, Used: 120, Limit: 100}, 80},
		{"overage exhausted", &kiroCreditUsage{OverageCap: 100, Used: 250, Limit: 100}, 0},
		{"used clamped at limit upstream", &kiroCreditUsage{OverageCap: 100, Used: 100, Limit: 100}, 100},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, overageRemaining(c.c), "%s", c.name)
	}
}

func TestOverageActive(t *testing.T) {
	cases := []struct {
		name string
		u    *kiroUsage
		want bool
	}{
		{"nil", nil, false},
		{"empty status", &kiroUsage{}, false},
		{"disabled with cap", &kiroUsage{OverageStatus: "DISABLED", Credit: &kiroCreditUsage{OverageCap: 100}}, false},
		{"enabled", &kiroUsage{OverageStatus: "ENABLED"}, true},
		{"active lowercase", &kiroUsage{OverageStatus: "active"}, true},
		{"enabled with whitespace", &kiroUsage{OverageStatus: " ENABLED "}, true},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, overageActive(c.u), "%s", c.name)
	}
}

// Overage disabled (default): zero base credit is parked even with overage left.
func TestSelectorApplyUsageOverageDisabledParks(t *testing.T) {
	s := newTestSelector(t, "a")

	s.applyUsage("a", &kiroUsage{Credit: &kiroCreditUsage{Remaining: 0, Used: 120, Limit: 100, OverageCap: 100}}, time.Now(), false)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.True(t, depleted, "overage disabled -> base exhausted parks despite overage budget")
}

// Overage enabled with budget left: not parked.
func TestSelectorApplyUsageOverageEnabledKeeps(t *testing.T) {
	s := newTestSelector(t, "a")
	require.NoError(t, s.store.SetOverageEnabled("a", true))

	s.applyUsage("a", &kiroUsage{OverageStatus: "ENABLED", Credit: &kiroCreditUsage{Remaining: 0, Used: 120, Limit: 100, OverageCap: 100}}, time.Now(), false)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.False(t, depleted, "overage enabled with budget left is not parked")
}

// Overage enabled but overage also exhausted: parked.
func TestSelectorApplyUsageOverageEnabledButExhausted(t *testing.T) {
	s := newTestSelector(t, "a")
	require.NoError(t, s.store.SetOverageEnabled("a", true))

	s.applyUsage("a", &kiroUsage{Credit: &kiroCreditUsage{Remaining: 0, Used: 250, Limit: 100, OverageCap: 100}}, time.Now(), false)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.True(t, depleted, "overage enabled but exhausted is parked")
}

// Overage enabled but the account has no overage configured: parked.
func TestSelectorApplyUsageOverageEnabledNoCap(t *testing.T) {
	s := newTestSelector(t, "a")
	require.NoError(t, s.store.SetOverageEnabled("a", true))

	s.applyUsage("a", &kiroUsage{Credit: &kiroCreditUsage{Remaining: 0, Used: 100, Limit: 100, OverageCap: 0}}, time.Now(), false)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.True(t, depleted, "overage enabled but no cap configured is parked")
}

// Local overage opt-in alone is not enough: when upstream has overage DISABLED
// the account is parked on base exhaustion (AWS would reject it). A DISABLED
// account still reports a would-be cap, so cap>0 must not lift it.
func TestSelectorApplyUsageOverageAwsDisabledParks(t *testing.T) {
	s := newTestSelector(t, "a")
	require.NoError(t, s.store.SetOverageEnabled("a", true))

	s.applyUsage("a", &kiroUsage{OverageStatus: "DISABLED", Credit: &kiroCreditUsage{Remaining: 0, Used: 120, Limit: 100, OverageCap: 100}}, time.Now(), false)

	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.True(t, depleted, "AWS overage disabled -> parked despite local opt-in and cap")
}

// Overage-enabled, base-exhausted account stays selectable end-to-end.
func TestSelectorPickOverageEnabledNotSkipped(t *testing.T) {
	s := newTestSelector(t, "a")
	require.NoError(t, s.store.SetOverageEnabled("a", true))
	s.applyUsage("a", &kiroUsage{OverageStatus: "ENABLED", Credit: &kiroCreditUsage{Remaining: 0, Used: 120, Limit: 100, OverageCap: 100}}, time.Now(), false)

	creds, ok := s.pick(map[string]bool{})
	require.True(t, ok)
	assert.Equal(t, "a", creds.id, "overage-enabled account with overage left is still picked")
}

// A probe (preciseOnly) must not lift an overage-exhausted account whose upstream
// currentUsage is clamped at the limit — that would override a fresher reactive
// 402 mark and loop (probe un-park -> fail -> re-park). Only an authoritative
// (non-probe) refresh lifts on the overage opt-in.
func TestSelectorApplyUsageProbeDoesNotLiftClampedOverage(t *testing.T) {
	s := newTestSelector(t, "a")
	require.NoError(t, s.store.SetOverageEnabled("a", true))
	// Park via the reactive path (a real 402), as openStream would.
	s.markDepleted("a", time.Now().Add(depletedFallbackTTL))
	// Upstream clamps currentUsage at the limit: Remaining=0 but OverageCap>0,
	// so overageRemaining is optimistically positive even with overage gone.
	clamped := &kiroUsage{OverageStatus: "ENABLED", Credit: &kiroCreditUsage{Remaining: 0, Used: 100, Limit: 100, OverageCap: 100}}

	// Probe (preciseOnly=true) with a newer fetched time must NOT lift.
	s.applyUsage("a", clamped, time.Now().Add(time.Minute), true)
	s.mu.Lock()
	_, depleted := s.depleted["a"]
	s.mu.Unlock()
	assert.True(t, depleted, "probe must not lift a clamped overage-exhausted account over a reactive mark")

	// An authoritative refresh (preciseOnly=false) still lifts: the operator
	// opted into overage, and outside the probe path the optimistic budget is
	// the best signal available (the reactive path re-parks if it's wrong).
	s.applyUsage("a", clamped, time.Now().Add(2*time.Minute), false)
	s.mu.Lock()
	_, depleted = s.depleted["a"]
	s.mu.Unlock()
	assert.False(t, depleted, "authoritative path still lifts an overage-opted account")
}

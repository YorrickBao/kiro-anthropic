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
	return newTestSelectorWithOverage(t, true, ids...)
}

func newStrictTestSelector(t *testing.T, ids ...string) *accountSelector {
	t.Helper()
	return newTestSelectorWithOverage(t, false, ids...)
}

func newTestSelectorWithOverage(t *testing.T, overage bool, ids ...string) *accountSelector {
	t.Helper()
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	for i, id := range ids {
		require.NoError(t, store.Add(&StoredAccount{
			ID: id, ClientID: "c", ClientSecret: "s", RefreshToken: "r",
			Region: "us-east-1", OverageEnabled: overage,
			CreatedAt: string(rune('0' + i)),
		}))
	}
	return newAccountSelector(store, &http.Client{})
}

func requireLease(t *testing.T, result pickResult) *accountLease {
	t.Helper()
	require.NotNil(t, result.lease)
	assert.Empty(t, result.verifyID)
	return result.lease
}

func applyFreshUsage(t *testing.T, s *accountSelector, id string, u *kiroUsage, source usageObservationSource) bool {
	t.Helper()
	_, stamp, ok := s.usageTarget(id)
	require.True(t, ok)
	return s.applyUsage(stamp, u, source)
}

func TestSelectorRoundRobin(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c")
	var got []string
	for i := 0; i < 6; i++ {
		lease := requireLease(t, s.pick(map[string]bool{}))
		got = append(got, lease.creds.id)
		s.recordSuccess(lease)
	}
	assert.Equal(t, []string{"a", "b", "c", "a", "b", "c"}, got)
}

func TestSelectorSessionAffinityIsStable(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c")
	const sessionID = "550e8400-e29b-41d4-a716-446655440000"

	first := requireLease(t, s.pickFor(map[string]bool{}, sessionID))
	for i := 0; i < 6; i++ {
		lease := requireLease(t, s.pickFor(map[string]bool{}, sessionID))
		assert.Equal(t, first.creds.id, lease.creds.id)
		s.recordSuccess(lease)
	}

	tried := map[string]bool{first.creds.id: true}
	second := requireLease(t, s.pickFor(tried, sessionID))
	assert.NotEqual(t, first.creds.id, second.creds.id)
	assert.Equal(t, second.creds.id, requireLease(t, s.pickFor(tried, sessionID)).creds.id)
}

func TestSelectorAffinityDoesNotAdvanceRoundRobin(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c")
	_ = requireLease(t, s.pickFor(map[string]bool{}, "session-affinity"))
	_ = requireLease(t, s.pickFor(map[string]bool{}, "session-affinity"))

	assert.Equal(t, "a", requireLease(t, s.pickFor(map[string]bool{}, "")).creds.id)
	assert.Equal(t, "b", requireLease(t, s.pick(map[string]bool{})).creds.id)
}

func TestSelectorAffinityHonorsQuotaAndCooldownGates(t *testing.T) {
	t.Run("depleted", func(t *testing.T) {
		s := newTestSelector(t, "a", "b")
		const sessionID = "depleted-session"
		preferred := requireLease(t, s.pickFor(map[string]bool{}, sessionID))
		s.recordDepleted(preferred)

		next := requireLease(t, s.pickFor(map[string]bool{}, sessionID))
		assert.NotEqual(t, preferred.creds.id, next.creds.id)
	})

	t.Run("cooldown", func(t *testing.T) {
		s := newTestSelector(t, "a", "b")
		const sessionID = "cooldown-session"
		preferred := requireLease(t, s.pickFor(map[string]bool{}, sessionID))
		s.recordFailure(preferred)

		next := requireLease(t, s.pickFor(map[string]bool{}, sessionID))
		assert.NotEqual(t, preferred.creds.id, next.creds.id)
		assert.False(t, next.fallback)
		s.recordFailure(next)

		fallback := requireLease(t, s.pickFor(map[string]bool{}, sessionID))
		assert.True(t, fallback.fallback)
		assert.Equal(t, preferred.creds.id, fallback.creds.id)
	})

	t.Run("strict unknown", func(t *testing.T) {
		s := newStrictTestSelector(t, "unknown", "available")
		assert.True(t, applyFreshUsage(t, s, "available", &kiroUsage{
			Credit: &kiroCreditUsage{Remaining: 1},
		}, usageObservationAuthoritative))

		available := requireLease(t, s.pickFor(map[string]bool{}, "strict-session"))
		assert.Equal(t, "available", available.creds.id)
		result := s.pickFor(map[string]bool{"available": true}, "strict-session")
		assert.Nil(t, result.lease)
		assert.Equal(t, "unknown", result.verifyID)
	})
}

func TestSelectorPeekAnyDoesNotAdvance(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	for i := 0; i < 5; i++ {
		creds, ok := s.peekAny()
		require.True(t, ok)
		assert.Equal(t, "a", creds.id)
	}
	assert.Equal(t, "a", requireLease(t, s.pick(map[string]bool{})).creds.id)
	assert.Equal(t, "b", requireLease(t, s.pick(map[string]bool{})).creds.id)
}

func TestSelectorEmptyAndAllTried(t *testing.T) {
	empty := newTestSelector(t)
	assert.Nil(t, empty.pick(map[string]bool{}).lease)
	assert.Empty(t, empty.pick(map[string]bool{}).verifyID)
	_, ok := empty.peekAny()
	assert.False(t, ok)

	s := newTestSelector(t, "a", "b")
	result := s.pick(map[string]bool{"a": true, "b": true})
	assert.Nil(t, result.lease)
	assert.Empty(t, result.verifyID)
}

func TestSelectorCooldownSkipAndFallback(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	first := requireLease(t, s.pick(map[string]bool{}))
	s.recordFailure(first)
	next := requireLease(t, s.pick(map[string]bool{}))
	assert.NotEqual(t, first.creds.id, next.creds.id)
	s.recordFailure(next)

	fallback := requireLease(t, s.pick(map[string]bool{}))
	assert.True(t, fallback.fallback)
	s.recordSuccess(fallback)
	assert.True(t, s.revalidate(requireLease(t, s.pick(map[string]bool{}))))
}

func TestSelectorCooldownFallbackUsesSoonest(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	_, _, ok := s.usageTarget("a")
	require.True(t, ok)
	_, _, ok = s.usageTarget("b")
	require.True(t, ok)
	s.mu.Lock()
	now := time.Now()
	s.states["a"].cooldownUntil = now.Add(10 * time.Minute)
	s.mutateLocked(s.states["a"])
	s.states["b"].cooldownUntil = now.Add(time.Minute)
	s.mutateLocked(s.states["b"])
	s.mu.Unlock()

	lease := requireLease(t, s.pick(map[string]bool{}))
	assert.Equal(t, "b", lease.creds.id)
	assert.True(t, lease.fallback)
}

func TestSelectorStatePrunedForRemovedAccount(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	lease := requireLease(t, s.pick(map[string]bool{}))
	s.recordFailure(lease)
	require.NoError(t, s.store.Remove(lease.creds.id))
	_ = s.pick(map[string]bool{})
	s.mu.Lock()
	_, exists := s.states[lease.creds.id]
	s.mu.Unlock()
	assert.False(t, exists)
}

func TestSelectorSkipsUnusableAndDisabledAccounts(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{ID: "dead", OverageEnabled: true, CreatedAt: "0"}))
	require.NoError(t, store.Add(&StoredAccount{
		ID: "off", ProfileArn: "arn:off", OverageEnabled: true, Disabled: true, CreatedAt: "1",
	}))
	require.NoError(t, store.Add(&StoredAccount{
		ID: "good", ProfileArn: "arn:good", OverageEnabled: true, CreatedAt: "2",
	}))
	s := newAccountSelector(store, &http.Client{})

	for i := 0; i < 4; i++ {
		assert.Equal(t, "good", requireLease(t, s.pick(map[string]bool{})).creds.id)
	}
	assert.False(t, accountUsable(StoredAccount{ProfileArn: "arn:x", Disabled: true}))
	assert.False(t, accountUsable(StoredAccount{}))
	assert.True(t, accountUsable(StoredAccount{ProfileArn: "arn:x"}))
}

func TestSelectorConcurrentPick(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c", "d")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lease := s.pick(map[string]bool{}).lease; lease != nil {
				s.recordSuccess(lease)
			}
		}()
	}
	wg.Wait()
}

func TestSelectorConcurrentSessionAffinityIsStable(t *testing.T) {
	s := newTestSelector(t, "a", "b", "c", "d")
	const sessionID = "concurrent-session"
	want := requireLease(t, s.pickFor(map[string]bool{}, sessionID)).creds.id

	const workers = 50
	got := make(chan string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease := s.pickFor(map[string]bool{}, sessionID).lease
			if lease != nil {
				got <- lease.creds.id
			}
		}()
	}
	wg.Wait()
	close(got)
	count := 0
	for id := range got {
		assert.Equal(t, want, id)
		count++
	}
	assert.Equal(t, workers, count)
}

func TestSelectorStrictUnknownRequiresVerification(t *testing.T) {
	s := newStrictTestSelector(t, "a")
	result := s.pick(map[string]bool{})
	assert.Nil(t, result.lease)
	assert.Equal(t, "a", result.verifyID)
	assert.Contains(t, s.probeIDs(), "a")
}

func TestSelectorFreshPositiveUsageAdmitsStrictAccount(t *testing.T) {
	s := newStrictTestSelector(t, "a")
	assert.True(t, applyFreshUsage(t, s, "a", &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 1},
	}, usageObservationAuthoritative))
	lease := requireLease(t, s.pick(map[string]bool{}))
	assert.Equal(t, "a", lease.creds.id)
	assert.True(t, s.revalidate(lease))
}

func TestSelectorStrictZeroUsageRemainsBlockedAfterRetryTime(t *testing.T) {
	s := newStrictTestSelector(t, "a")
	assert.True(t, applyFreshUsage(t, s, "a", &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 0},
	}, usageObservationAuthoritative))

	s.mu.Lock()
	s.states["a"].retryAfter = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	result := s.pick(map[string]bool{})
	assert.Nil(t, result.lease)
	assert.Empty(t, result.verifyID, "known depletion is not treated as unknown")
	assert.True(t, s.isDepleted("a"))
}

func TestSelectorRuntimeSuccessCannotAdmitUnknownOrDepleted(t *testing.T) {
	s := newStrictTestSelector(t, "a")
	creds, stamp, ok := s.usageTarget("a")
	require.True(t, ok)
	unknownLease := &accountLease{creds: creds, revision: stamp.revision, generation: stamp.generation}
	s.recordSuccess(unknownLease)
	assert.Equal(t, "a", s.pick(map[string]bool{}).verifyID)

	assert.True(t, s.applyUsage(stamp, &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 0},
	}, usageObservationAuthoritative))
	_, depletedStamp, ok := s.usageTarget("a")
	require.True(t, ok)
	depletedLease := &accountLease{creds: creds, revision: depletedStamp.revision, generation: depletedStamp.generation}
	s.recordSuccess(depletedLease)
	assert.Nil(t, s.pick(map[string]bool{}).lease)
	assert.True(t, s.isDepleted("a"))
}

func TestSelectorOverageUnknownIsEligible(t *testing.T) {
	s := newTestSelector(t, "a")
	lease := requireLease(t, s.pick(map[string]bool{}))
	assert.Equal(t, "a", lease.creds.id)
	assert.Equal(t, quotaUnknown, selectorQuota(t, s, "a"))
}

func TestSelectorReactiveDepletionIsStickyAndNeverFallback(t *testing.T) {
	s := newTestSelector(t, "a", "b")
	a := requireLease(t, s.pick(map[string]bool{}))
	b := requireLease(t, s.pick(map[string]bool{}))
	s.recordDepleted(a)
	s.recordDepleted(b)

	result := s.pick(map[string]bool{})
	assert.Nil(t, result.lease)
	assert.Empty(t, result.verifyID)
	assert.True(t, s.isReactivelyDepleted("a", a.revision))
	assert.True(t, s.isReactivelyDepleted("b", b.revision))
}

func TestSelectorOldSuccessCannotClearNewerDepletion(t *testing.T) {
	s := newTestSelector(t, "a")
	lease := requireLease(t, s.pick(map[string]bool{}))
	s.recordDepleted(lease)
	s.recordSuccess(lease)
	assert.True(t, s.isReactivelyDepleted("a", lease.revision))
	assert.Nil(t, s.pick(map[string]bool{}).lease)
}

func TestSelectorStaleUsageCannotClearReactiveDepletion(t *testing.T) {
	s := newTestSelector(t, "a")
	creds, stamp, ok := s.usageTarget("a")
	require.True(t, ok)
	lease := &accountLease{creds: creds, revision: stamp.revision, generation: stamp.generation}
	s.recordDepleted(lease)

	accepted := s.applyUsage(stamp, &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 10},
	}, usageObservationAuthoritative)
	assert.False(t, accepted)
	assert.True(t, s.isReactivelyDepleted("a", stamp.revision))
}

func TestSelectorFreshBaseUsageRecoversReactiveDepletion(t *testing.T) {
	s := newTestSelector(t, "a")
	lease := requireLease(t, s.pick(map[string]bool{}))
	s.recordDepleted(lease)
	assert.True(t, applyFreshUsage(t, s, "a", &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 1},
	}, usageObservationProbe))
	assert.NotNil(t, s.pick(map[string]bool{}).lease)
}

func TestSelectorBaseZeroCannotRecoverReactiveOverage(t *testing.T) {
	s := newTestSelector(t, "a")
	lease := requireLease(t, s.pick(map[string]bool{}))
	s.recordDepleted(lease)
	clamped := &kiroUsage{
		OverageStatus: "ENABLED",
		Credit:        &kiroCreditUsage{Remaining: 0, Used: 100, Limit: 100, OverageCap: 100},
	}
	assert.True(t, applyFreshUsage(t, s, "a", clamped, usageObservationAuthoritative))
	assert.Nil(t, s.pick(map[string]bool{}).lease)
	assert.True(t, s.isReactivelyDepleted("a", lease.revision))
}

func TestSelectorOverageUsageCanAuthorizeBeforeReactiveFailure(t *testing.T) {
	s := newTestSelector(t, "a")
	assert.True(t, applyFreshUsage(t, s, "a", &kiroUsage{
		OverageStatus: "ENABLED",
		Credit:        &kiroCreditUsage{Remaining: 0, Used: 120, Limit: 100, OverageCap: 100},
	}, usageObservationAuthoritative))
	assert.Equal(t, quotaAvailable, selectorQuota(t, s, "a"))
	assert.NotNil(t, s.pick(map[string]bool{}).lease)
}

func TestSelectorDisablingOverageRequiresFreshPositiveBaseUsage(t *testing.T) {
	s := newTestSelector(t, "a")
	assert.True(t, applyFreshUsage(t, s, "a", &kiroUsage{
		OverageStatus: "ENABLED",
		Credit:        &kiroCreditUsage{Remaining: 0, Used: 120, Limit: 100, OverageCap: 100},
	}, usageObservationAuthoritative))
	old := requireLease(t, s.pick(map[string]bool{}))

	require.NoError(t, s.store.SetOverageEnabled("a", false))
	assert.False(t, s.revalidate(old))
	result := s.pick(map[string]bool{})
	assert.Nil(t, result.lease)
	assert.Equal(t, "a", result.verifyID)
	assert.Equal(t, quotaUnknown, selectorQuota(t, s, "a"))

	assert.True(t, applyFreshUsage(t, s, "a", &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 1},
	}, usageObservationAuthoritative))
	assert.NotNil(t, s.pick(map[string]bool{}).lease)
}

func TestSelectorPolicyRevisionRejectsOldUsageAndLease(t *testing.T) {
	s := newTestSelector(t, "a")
	creds, stamp, ok := s.usageTarget("a")
	require.True(t, ok)
	lease := &accountLease{creds: creds, revision: stamp.revision, generation: stamp.generation}
	require.NoError(t, s.store.SetOverageEnabled("a", false))

	assert.False(t, s.applyUsage(stamp, &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 10},
	}, usageObservationAuthoritative))
	s.recordDepleted(lease)
	assert.False(t, s.revalidate(lease))
	result := s.pick(map[string]bool{})
	assert.Nil(t, result.lease)
	assert.Equal(t, "a", result.verifyID)
}

func TestSelectorRemoveReaddRejectsOldLeaseEvents(t *testing.T) {
	s := newTestSelector(t, "a")
	old := requireLease(t, s.pick(map[string]bool{}))
	require.NoError(t, s.store.Remove("a"))
	require.NoError(t, s.store.Add(&StoredAccount{
		ID: "a", ProfileArn: "arn:new", OverageEnabled: true, CreatedAt: "0",
	}))
	fresh := requireLease(t, s.pick(map[string]bool{}))
	require.NotEqual(t, old.revision, fresh.revision)

	s.recordDepleted(old)
	assert.False(t, s.isReactivelyDepleted("a", fresh.revision))
	assert.True(t, s.revalidate(fresh))
}

func TestSelectorRevalidateRejectsChangedState(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		s := newTestSelector(t, "a")
		lease := requireLease(t, s.pick(map[string]bool{}))
		require.NoError(t, s.store.SetDisabled("a", true))
		assert.False(t, s.revalidate(lease))
	})
	t.Run("removed", func(t *testing.T) {
		s := newTestSelector(t, "a")
		lease := requireLease(t, s.pick(map[string]bool{}))
		require.NoError(t, s.store.Remove("a"))
		assert.False(t, s.revalidate(lease))
	})
	t.Run("depleted", func(t *testing.T) {
		s := newTestSelector(t, "a")
		lease := requireLease(t, s.pick(map[string]bool{}))
		s.recordDepleted(lease)
		assert.False(t, s.revalidate(lease))
	})
	t.Run("generation changed", func(t *testing.T) {
		s := newTestSelector(t, "a")
		lease := requireLease(t, s.pick(map[string]bool{}))
		s.recordFailure(lease)
		assert.False(t, s.revalidate(lease))
	})
}

func TestSelectorProbeIDs(t *testing.T) {
	s := newStrictTestSelector(t, "unknown", "depleted", "available")
	assert.True(t, applyFreshUsage(t, s, "depleted", &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 0},
	}, usageObservationAuthoritative))
	assert.True(t, applyFreshUsage(t, s, "available", &kiroUsage{
		Credit: &kiroCreditUsage{Remaining: 1},
	}, usageObservationAuthoritative))
	assert.ElementsMatch(t, []string{"unknown", "depleted"}, s.probeIDs())
}

func TestSelectorProbeIDsIncludesNonReactiveDepletedOverage(t *testing.T) {
	s := newTestSelector(t, "overage")
	assert.True(t, applyFreshUsage(t, s, "overage", &kiroUsage{
		OverageStatus: "DISABLED",
		Credit:        &kiroCreditUsage{Remaining: 0},
	}, usageObservationAuthoritative))
	assert.Equal(t, quotaDepleted, selectorQuota(t, s, "overage"))
	revision := runtimeRevision(t, s.store, "overage")
	assert.False(t, s.isReactivelyDepleted("overage", revision))
	assert.Contains(t, s.probeIDs(), "overage")
}

func selectorQuota(t *testing.T, s *accountSelector, id string) quotaEligibility {
	t.Helper()
	rt, ok := s.store.Runtime(id)
	require.True(t, ok)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[id]
	require.NotNil(t, st)
	require.Equal(t, rt.Revision, st.revision)
	return st.quota
}

func TestIsAccountFailure(t *testing.T) {
	assert.False(t, isAccountFailure(nil))
	assert.True(t, isAccountFailure(assertAnError{}))
	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusPaymentRequired, true},
		{http.StatusTooManyRequests, true},
		{http.StatusLocked, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, isAccountFailure(&kiroHTTPError{Status: tc.status}), "status %d", tc.status)
	}
}

type assertAnError struct{}

func (assertAnError) Error() string { return "boom" }

func TestIsAccountDepleted(t *testing.T) {
	assert.False(t, isAccountDepleted(nil))
	assert.False(t, isAccountDepleted(assertAnError{}))
	assert.True(t, isAccountDepleted(&kiroHTTPError{Status: http.StatusPaymentRequired}))
	assert.True(t, isAccountDepleted(&kiroHTTPError{Status: http.StatusLocked}))
	assert.True(t, isAccountDepleted(&kiroHTTPError{Kind: "serviceQuotaExceededError"}))
	assert.False(t, isAccountDepleted(&kiroHTTPError{Status: http.StatusTooManyRequests}))
}

func TestOverageRemaining(t *testing.T) {
	cases := []struct {
		name string
		c    *kiroCreditUsage
		want float64
	}{
		{"nil", nil, 0},
		{"no cap", &kiroCreditUsage{OverageCap: 0, Used: 5, Limit: 10}, 0},
		{"not in overage", &kiroCreditUsage{OverageCap: 100, Used: 5, Limit: 10}, 100},
		{"spending", &kiroCreditUsage{OverageCap: 100, Used: 120, Limit: 100}, 80},
		{"exhausted", &kiroCreditUsage{OverageCap: 100, Used: 250, Limit: 100}, 0},
		{"clamped", &kiroCreditUsage{OverageCap: 100, Used: 100, Limit: 100}, 100},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, overageRemaining(tc.c), tc.name)
	}
}

func TestOverageActive(t *testing.T) {
	assert.False(t, overageActive(nil))
	assert.False(t, overageActive(&kiroUsage{OverageStatus: "DISABLED"}))
	assert.True(t, overageActive(&kiroUsage{OverageStatus: "ENABLED"}))
	assert.True(t, overageActive(&kiroUsage{OverageStatus: " active "}))
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newProbeTestServer(t *testing.T, handler http.HandlerFunc, ids ...string) *Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for i, id := range ids {
		require.NoError(t, store.Add(&StoredAccount{
			ID: id, ClientID: "client", ClientSecret: "secret", RefreshToken: "refresh",
			AccessToken: id, ExpiresAt: future, Region: "us-east-1",
			ProfileArn: "arn:" + id, CreatedAt: time.Now().Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339),
		}))
	}
	s := NewServer(&Config{}, client)
	s.setAccounts(store, client)
	return s
}

func probeRequestID(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func writeOverageUsageResponse(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"overageConfiguration": map[string]any{"overageStatus": "ENABLED"},
		"usageBreakdownList": []map[string]any{{
			"resourceType": "CREDIT", "currentUsageWithPrecision": 120,
			"usageLimitWithPrecision": 100, "overageCapWithPrecision": 100,
		}},
	})
}

func cleanupTestRelease(t *testing.T, release chan struct{}) func() {
	t.Helper()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return unblock
}

func TestDepletedProbeRunScansImmediately(t *testing.T) {
	called := make(chan struct{}, 1)
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeUsageResponse(w, 50)
		called <- struct{}{}
	}, "acc")
	p := newDepletedProbe(s, nil)
	p.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("initial probe did not run immediately")
	}
	require.Eventually(t, func() bool {
		return selectorQuota(t, s.selector, "acc") == quotaBase
	}, time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe Run did not stop after cancellation")
	}
}

func TestDepletedProbeBoundsConcurrency(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	var calls atomic.Int32
	started := make(chan string, 8)
	release := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- probeRequestID(r)
		<-release
		writeUsageResponse(w, 50)
	}, "a", "b", "c", "d", "e", "f")
	unblock := cleanupTestRelease(t, release)
	p := newDepletedProbe(s, nil)
	p.concurrency = 2
	done := make(chan struct{})
	go func() {
		p.scan(context.Background())
		close(done)
	}()

	<-started
	<-started
	select {
	case id := <-started:
		t.Fatalf("third account %q started above concurrency cap", id)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Equal(t, int32(2), peak.Load())
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded scan did not finish")
	}
	assert.Equal(t, int32(6), calls.Load())
	assert.LessOrEqual(t, peak.Load(), int32(2))
}

func TestDepletedProbeLimiterUsesConfiguredCapacityAfterSmallFirstScan(t *testing.T) {
	var blocking atomic.Bool
	started := make(chan string, 3)
	release := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if blocking.Load() {
			started <- probeRequestID(r)
			<-release
		}
		writeUsageResponse(w, 50)
	}, "a")
	unblock := cleanupTestRelease(t, release)
	p := newDepletedProbe(s, nil)
	p.concurrency = 3

	// The first scan has one target but must not permanently create a one-slot
	// limiter: later scans can have more targets after accounts are added.
	p.scan(context.Background())
	require.NotNil(t, p.fetchLimiter)
	require.Equal(t, 3, cap(p.fetchLimiter))

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for i, id := range []string{"b", "c", "d"} {
		require.NoError(t, s.accounts.Add(&StoredAccount{
			ID: id, ClientID: "client", ClientSecret: "secret", RefreshToken: "refresh",
			AccessToken: id, ExpiresAt: future, Region: "us-east-1",
			ProfileArn: "arn:" + id, CreatedAt: time.Now().Add(time.Duration(i+1) * time.Second).UTC().Format(time.RFC3339),
		}))
	}
	blocking.Store(true)
	done := make(chan struct{})
	go func() {
		p.scan(context.Background())
		close(done)
	}()

	seen := make([]string, 0, 3)
	for len(seen) < 3 {
		select {
		case id := <-started:
			seen = append(seen, id)
		case <-time.After(time.Second):
			t.Fatalf("only %d later targets started with configured concurrency 3: %v", len(seen), seen)
		}
	}
	assert.ElementsMatch(t, []string{"b", "c", "d"}, seen)
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("later scan did not finish after releasing fetches")
	}
}

func TestDepletedProbeLimitsLiveFetchesAfterWaiterTimeout(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	var calls atomic.Int32
	started := make(chan string, 2)
	release := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		id := probeRequestID(r)
		started <- id
		if id == "a" {
			<-release
		}
		writeUsageResponse(w, 50)
	}, "a", "b")
	unblock := cleanupTestRelease(t, release)
	p := newDepletedProbe(s, nil)
	p.concurrency = 1
	p.timeout = 30 * time.Millisecond
	done := make(chan struct{})
	go func() {
		p.scan(context.Background())
		close(done)
	}()

	assert.Equal(t, "a", <-started)
	select {
	case id := <-started:
		t.Fatalf("second account %q fetched while the timed-out first fetch was still live", id)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case id := <-started:
		assert.Equal(t, "b", id)
	case <-time.After(time.Second):
		t.Fatal("second account did not fetch after the first live fetch released its slot")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan did not return after waiter timeouts")
	}
	require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, int32(1), peak.Load())
}

func TestDepletedProbePerAccountTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writeUsageResponse(w, 50)
	}, "acc")
	unblock := cleanupTestRelease(t, release)
	p := newDepletedProbe(s, nil)
	p.timeout = 30 * time.Millisecond

	begin := time.Now()
	p.scan(context.Background())
	assert.Less(t, time.Since(begin), 500*time.Millisecond)
	assert.Equal(t, quotaUnknown, selectorQuota(t, s.selector, "acc"))
	unblock()
	<-started
	require.Never(t, func() bool {
		return selectorQuota(t, s.selector, "acc") == quotaBase
	}, 100*time.Millisecond, 10*time.Millisecond)
}

func TestDepletedProbeHonorsParentCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writeUsageResponse(w, 50)
	}, "acc")
	unblock := cleanupTestRelease(t, release)
	p := newDepletedProbe(s, nil)
	p.timeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.scan(ctx)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("scan did not return promptly after parent cancellation")
	}
	unblock()
	assert.Equal(t, quotaUnknown, selectorQuota(t, s.selector, "acc"))
}

func TestDepletedProbeContinuesPastStalledAccount(t *testing.T) {
	stallStarted := make(chan struct{})
	fast := make(chan string, 2)
	releaseStall := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		id := probeRequestID(r)
		if id == "a" {
			close(stallStarted)
			<-releaseStall
		} else {
			fast <- id
		}
		writeUsageResponse(w, 50)
	}, "a", "b", "c")
	unblockStall := cleanupTestRelease(t, releaseStall)
	p := newDepletedProbe(s, nil)
	p.concurrency = 2
	p.timeout = time.Second
	done := make(chan struct{})
	go func() {
		p.scan(context.Background())
		close(done)
	}()
	<-stallStarted

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-fast:
			seen[id] = true
		case <-time.After(500 * time.Millisecond):
			t.Fatal("healthy accounts were blocked behind stalled account")
		}
	}
	assert.True(t, seen["b"])
	assert.True(t, seen["c"])
	unblockStall()
	<-done
}

func TestDepletedProbeRecoversPositiveUsage(t *testing.T) {
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeUsageResponse(w, 50)
	}, "acc")
	newDepletedProbe(s, nil).scan(context.Background())

	assert.Equal(t, quotaBase, selectorQuota(t, s.selector, "acc"))
	lease := requireLease(t, s.selector.pick(map[string]bool{}))
	assert.Equal(t, "acc", lease.creds.id)
}

func TestDepletedProbeRetainsZeroAndErrorTargets(t *testing.T) {
	s := newProbeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if probeRequestID(r) == "error" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeUsageResponse(w, 0)
	}, "zero", "error")
	newDepletedProbe(s, nil).scan(context.Background())

	assert.Equal(t, quotaDepleted, selectorQuota(t, s.selector, "zero"))
	assert.Equal(t, quotaUnknown, selectorQuota(t, s.selector, "error"))
	assert.ElementsMatch(t, []string{"zero", "error"}, reconciliationTargetIDs(s.selector))
	picked := s.selector.pick(map[string]bool{})
	assert.Nil(t, picked.lease)
}

func TestDepletedProbeRecoversStickyReactiveTarget(t *testing.T) {
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeUsageResponse(w, 50)
	}, "acc")
	require.NoError(t, s.accounts.SetOverageEnabled("acc", true))
	lease := requireLease(t, s.selector.pick(map[string]bool{}))
	s.selector.recordDepleted(lease)
	require.Contains(t, reconciliationTargetIDs(s.selector), "acc")

	newDepletedProbe(s, nil).scan(context.Background())
	assert.Equal(t, quotaBase, selectorQuota(t, s.selector, "acc"))
	assert.False(t, s.selector.isReactivelyDepleted("acc", runtimeRevision(t, s.accounts, "acc")))
}

func TestDepletedProbeRecoversNonReactiveOverageTarget(t *testing.T) {
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeUsageResponse(w, 50)
	}, "acc")
	require.NoError(t, s.accounts.SetOverageEnabled("acc", true))
	assert.True(t, applyFreshUsage(t, s.selector, "acc", &kiroUsage{
		OverageStatus: "DISABLED",
		Credit:        &kiroCreditUsage{Remaining: 0},
	}, usageObservationAuthoritative))
	revision := runtimeRevision(t, s.accounts, "acc")
	assert.Equal(t, quotaDepleted, selectorQuota(t, s.selector, "acc"))
	assert.False(t, s.selector.isReactivelyDepleted("acc", revision))
	require.Contains(t, reconciliationTargetIDs(s.selector), "acc")

	newDepletedProbe(s, nil).scan(context.Background())
	assert.Equal(t, quotaBase, selectorQuota(t, s.selector, "acc"))
	assert.NotNil(t, s.selector.pick(map[string]bool{}).lease)
}

func TestDepletedProbeReclassifiesOverageEnabledUnknown(t *testing.T) {
	var calls atomic.Int32
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeOverageUsageResponse(w)
	}, "acc")
	require.NoError(t, s.accounts.SetOverageEnabled("acc", true))

	newDepletedProbe(s, nil).scan(context.Background())

	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, quotaOverage, selectorQuota(t, s.selector, "acc"))
}

func TestDepletedProbeReclassifiesBaseAsOverageAndBypassesCache(t *testing.T) {
	var calls atomic.Int32
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeOverageUsageResponse(w)
	}, "acc")
	require.NoError(t, s.accounts.SetOverageEnabled("acc", true))
	require.True(t, applyFreshUsage(t, s.selector, "acc", testBaseUsage(), usageObservationAuthoritative))
	revision := runtimeRevision(t, s.accounts, "acc")
	s.usageMu.Lock()
	s.usageCache["acc"] = usageCacheEntry{
		usage:   &kiroUsage{Credit: &kiroCreditUsage{Remaining: 99}},
		fetched: time.Now(), revision: revision,
	}
	s.usageMu.Unlock()

	newDepletedProbe(s, nil).scan(context.Background())

	assert.Equal(t, int32(1), calls.Load(), "periodic reconciliation must bypass a fresh Usage cache entry")
	assert.Equal(t, quotaOverage, selectorQuota(t, s.selector, "acc"))
}

func TestDepletedProbeReclassifiesOverageAsBase(t *testing.T) {
	var calls atomic.Int32
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeUsageResponse(w, 50)
	}, "acc")
	require.NoError(t, s.accounts.SetOverageEnabled("acc", true))
	require.True(t, applyFreshUsage(t, s.selector, "acc", testOverageUsage(), usageObservationAuthoritative))

	newDepletedProbe(s, nil).scan(context.Background())

	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, quotaBase, selectorQuota(t, s.selector, "acc"))
}

func TestDepletedProbeKeepsReactiveDepletionStickyOnBaseZeroOverage(t *testing.T) {
	var calls atomic.Int32
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeOverageUsageResponse(w)
	}, "acc")
	require.NoError(t, s.accounts.SetOverageEnabled("acc", true))
	lease := requireLease(t, s.selector.pick(map[string]bool{}))
	s.selector.recordDepleted(lease)

	newDepletedProbe(s, nil).scan(context.Background())

	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, quotaDepleted, selectorQuota(t, s.selector, "acc"))
	assert.True(t, s.selector.isReactivelyDepleted("acc", runtimeRevision(t, s.accounts, "acc")))
}

func TestDepletedProbeRejectsQueuedTargetAfterPolicyChange(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if probeRequestID(r) == "a" {
			close(started)
			<-release
		}
		writeUsageResponse(w, 50)
	}, "a", "b")
	unblock := cleanupTestRelease(t, release)
	p := newDepletedProbe(s, nil)
	p.concurrency = 1
	p.timeout = 30 * time.Millisecond
	done := make(chan struct{})
	go func() {
		p.scan(context.Background())
		close(done)
	}()
	<-started
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan did not release timed-out waiters")
	}
	// Target b has passed its initial stamp check and is queued behind a's live
	// fetch. Change its policy before that limiter slot becomes available.
	require.NoError(t, s.accounts.SetOverageEnabled("b", true))
	current, ok := s.accounts.Runtime("b")
	require.True(t, ok)
	unblock()
	require.Eventually(t, func() bool {
		s.selector.mu.Lock()
		defer s.selector.mu.Unlock()
		st := s.selector.states["b"]
		return st != nil && st.revision == current.Revision
	}, time.Second, 10*time.Millisecond)

	assert.Equal(t, int32(1), calls.Load(), "stale queued Probe target must not fetch under its old source")
	assert.Equal(t, quotaUnknown, selectorQuota(t, s.selector, "b"))
}

func TestDepletedProbeRejectsStaleRevisionResult(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := newProbeTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writeUsageResponse(w, 50)
	}, "acc")
	unblock := cleanupTestRelease(t, release)
	p := newDepletedProbe(s, nil)
	done := make(chan struct{})
	go func() {
		p.scan(context.Background())
		close(done)
	}()
	<-started
	fresh, ok := s.accounts.Get("acc")
	require.True(t, ok)
	fresh.AccessToken = "replacement"
	require.NoError(t, s.accounts.ReplaceCredentials("acc", &fresh))
	unblock()
	<-done

	picked := s.selector.pick(map[string]bool{})
	assert.Nil(t, picked.lease)
	assert.Equal(t, "acc", picked.verifyID)
	assert.Equal(t, quotaUnknown, selectorQuota(t, s.selector, "acc"))
	s.usageMu.Lock()
	_, cached := s.usageCache["acc"]
	s.usageMu.Unlock()
	assert.False(t, cached)
}

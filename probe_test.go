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
		return selectorQuota(t, s.selector, "acc") == quotaAvailable
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
		return selectorQuota(t, s.selector, "acc") == quotaAvailable
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

	assert.Equal(t, quotaAvailable, selectorQuota(t, s.selector, "acc"))
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
	assert.ElementsMatch(t, []string{"zero", "error"}, s.selector.probeIDs())
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
	require.Contains(t, s.selector.probeIDs(), "acc")

	newDepletedProbe(s, nil).scan(context.Background())
	assert.Equal(t, quotaAvailable, selectorQuota(t, s.selector, "acc"))
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
	require.Contains(t, s.selector.probeIDs(), "acc")

	newDepletedProbe(s, nil).scan(context.Background())
	assert.Equal(t, quotaAvailable, selectorQuota(t, s.selector, "acc"))
	assert.NotNil(t, s.selector.pick(map[string]bool{}).lease)
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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// testServerWithModels builds a Server whose model lookups resolve from a
// pre-seeded per-account cache (no network), with one account in the pool so
// anyModels can pick it.
func testServerWithModels() *Server {
	store, err := NewAccountStore(filepath.Join(os.TempDir(), "sfm-"+time.Now().Format("150405.000000")+".json"))
	if err != nil {
		panic(err)
	}
	_ = store.Add(&StoredAccount{ID: "acc", Region: "us-east-1", ProfileArn: "arn:test", CreatedAt: "1", OverageEnabled: true})
	rt, _ := store.Runtime("acc")
	s := &Server{
		cfg:         &Config{},
		selector:    newAccountSelector(store, &http.Client{}),
		accounts:    store,
		modelsCache: map[string]modelsCacheEntry{},
		usageCache:  map[string]usageCacheEntry{},
	}
	s.modelsCache["acc"] = modelsCacheEntry{
		models:   []kiroModelInfo{testOpusModel(), testSonnet45Model()},
		fetched:  time.Now(),
		revision: rt.Revision,
	}
	return s
}

func TestAggregateModelUsageExcludesReactivelyDepletedOverage(t *testing.T) {
	s := testServerWithModels()
	require.NoError(t, discardChanged(s.accounts.SetOverageEnabledChanged("acc", true)))

	// This is the ambiguous snapshot the selector explicitly handles: upstream
	// clamps currentUsage at the base limit, so the arithmetic heuristic still
	// sees the full overage cap even after a real request has exhausted it.
	s.usageCache["acc"] = usageCacheEntry{
		usage: &kiroUsage{
			OverageStatus: "ENABLED",
			Credit: &kiroCreditUsage{
				Used: 100, Limit: 100, Remaining: 0, OverageCap: 100,
			},
		},
		fetched:  time.Now(),
		revision: runtimeRevision(t, s.accounts, "acc"),
	}

	before := s.aggregateModelUsage(context.Background(), "claude-opus-4.8")
	require.Len(t, before.Accounts, 1, "aggregate: %+v", before)
	assert.True(t, before.Accounts[0].OnOverage)
	assert.Equal(t, float64(100), before.Totals.OverageCap)

	// A real depletion response is more authoritative than the optimistic
	// snapshot. Once parked, the account must disappear from the aggregate just
	// as it does from normal selector routing.
	picked := s.selector.pickFor(map[string]bool{}, "")
	require.NotNil(t, picked.lease)
	s.selector.recordDepleted(picked.lease)
	after := s.aggregateModelUsage(context.Background(), "claude-opus-4.8")
	assert.Empty(t, after.Accounts)
	assert.Equal(t, 0, after.Totals.Accounts)
	assert.Equal(t, float64(0), after.Totals.OverageCap)
	assert.Equal(t, 1, after.Excluded)
}

func newUsageBackedServer(t *testing.T, strict bool, handler http.HandlerFunc) *Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acc", ClientID: "client", ClientSecret: "secret", RefreshToken: "refresh",
		AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		Region: "us-east-1", ProfileArn: "arn:test", CreatedAt: "1", OverageEnabled: !strict,
	}))
	s := NewServer(&Config{}, client)
	s.setAccounts(store, client)
	return s
}

func writeUsageResponse(w http.ResponseWriter, remaining float64) {
	writeJSON(w, http.StatusOK, map[string]any{
		"overageConfiguration": map[string]any{"overageStatus": "DISABLED"},
		"usageBreakdownList": []map[string]any{{
			"resourceType": "CREDIT", "currentUsageWithPrecision": 100 - remaining,
			"usageLimitWithPrecision": 100,
		}},
	})
}

func TestUsageReadOnlyFetchDoesNotActivateStrictAccount(t *testing.T) {
	var calls atomic.Int32
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeUsageResponse(w, 50)
	})
	creds, ok := s.selector.byID("acc")
	require.True(t, ok)

	u, err := s.ensureUsageReadOnly(context.Background(), creds)
	require.NoError(t, err)
	require.Equal(t, float64(50), u.Credit.Remaining)
	assert.Equal(t, int32(1), calls.Load())
	picked := s.selector.pickFor(map[string]bool{}, "")
	assert.Nil(t, picked.lease)
	assert.Equal(t, "acc", picked.verifyID)
}

func TestUsageObservedAndReadOnlyWaitersShareFetch(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		writeUsageResponse(w, 50)
	})
	creds, ok := s.selector.byID("acc")
	require.True(t, ok)

	readErr := make(chan error, 1)
	go func() {
		_, err := s.ensureUsageReadOnly(context.Background(), creds)
		readErr <- err
	}()
	<-started
	observedErr := make(chan error, 1)
	go func() {
		_, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
		observedErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	require.NoError(t, <-readErr)
	require.NoError(t, <-observedErr)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, quotaBase, selectorQuota(t, s.selector, "acc"))
}

func TestUsageWaiterCancellationDoesNotCancelSharedFetch(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		writeUsageResponse(w, 50)
	})

	ctx, cancel := context.WithCancel(context.Background())
	canceledErr := make(chan error, 1)
	go func() {
		_, err := s.refreshUsage(ctx, "acc", usageObservationAuthoritative)
		canceledErr <- err
	}()
	<-started
	otherErr := make(chan error, 1)
	go func() {
		_, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
		otherErr <- err
	}()
	cancel()
	require.ErrorIs(t, <-canceledErr, context.Canceled)
	close(release)
	require.NoError(t, <-otherErr)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, quotaBase, selectorQuota(t, s.selector, "acc"))
}

func TestUsageFetchRetriesAfterAccountRevisionChanges(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			writeUsageResponse(w, 10)
			return
		}
		writeUsageResponse(w, 50)
	})
	unblock := cleanupTestRelease(t, release)

	result := make(chan *kiroUsage, 1)
	fetchErr := make(chan error, 1)
	go func() {
		u, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
		result <- u
		fetchErr <- err
	}()
	<-started
	fresh, ok := s.accounts.Get("acc")
	require.True(t, ok)
	fresh.AccessToken = "replacement"
	require.NoError(t, s.accounts.ReplaceCredentials("acc", &fresh))
	unblock()
	require.NoError(t, <-fetchErr)
	u := <-result
	require.NotNil(t, u)
	require.NotNil(t, u.Credit)
	assert.Equal(t, float64(50), u.Credit.Remaining)
	assert.Equal(t, int32(2), calls.Load())

	rt, ok := s.accounts.Runtime("acc")
	require.True(t, ok)
	s.usageMu.Lock()
	cached, exists := s.usageCache["acc"]
	s.usageMu.Unlock()
	require.True(t, exists)
	assert.Equal(t, rt.Revision, cached.revision)
	assert.Equal(t, float64(50), cached.usage.Credit.Remaining)
	assert.Equal(t, quotaBase, selectorQuota(t, s.selector, "acc"))
}

func TestAggregateModelUsageRejectsMixedAccountRevisions(t *testing.T) {
	modelsStarted := make(chan struct{})
	releaseModels := make(chan struct{})
	var usageCalls atomic.Int32
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") == "KiroControlPlaneBearerService.ListAvailableModels" {
			close(modelsStarted)
			<-releaseModels
			writeJSON(w, http.StatusOK, map[string]any{
				"models": []map[string]any{{"modelId": "claude-opus-4.8"}},
			})
			return
		}
		if r.URL.Path == "/getUsageLimits" {
			usageCalls.Add(1)
			writeUsageResponse(w, 0)
			return
		}
		http.NotFound(w, r)
	})

	aggCh := make(chan *modelAggregate, 1)
	go func() {
		aggCh <- s.aggregateModelUsage(context.Background(), "claude-opus-4.8")
	}()
	<-modelsStarted
	require.NoError(t, discardChanged(s.accounts.SetOverageEnabledChanged("acc", false)))
	close(releaseModels)
	agg := <-aggCh

	assert.Empty(t, agg.Accounts)
	assert.Equal(t, int32(0), usageCalls.Load(), "stale model snapshot must not fetch newer-revision usage")
	require.Len(t, agg.Errors, 1)
	assert.Contains(t, agg.Errors[0].Error, errAccountRevisionChanged.Error())
}

func TestUsageFetchRetriesWithNewGeneration(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			writeUsageResponse(w, 50)
			return
		}
		writeUsageResponse(w, 0)
	})
	unblock := cleanupTestRelease(t, release)
	lease := requireLease(t, s.selector.pickFor(map[string]bool{}, ""))
	result := make(chan *kiroUsage, 1)
	fetchErr := make(chan error, 1)
	go func() {
		u, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
		result <- u
		fetchErr <- err
	}()
	<-started
	s.selector.recordDepleted(lease)
	unblock()
	require.NoError(t, <-fetchErr)
	u := <-result
	require.NotNil(t, u)
	require.NotNil(t, u.Credit)
	assert.Equal(t, float64(0), u.Credit.Remaining)
	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, quotaDepleted, selectorQuota(t, s.selector, "acc"))
	assert.True(t, s.selector.isReactivelyDepleted("acc", lease.revision))
}

func TestUsageTargetStampMismatchDoesNotFetchOrRetry(t *testing.T) {
	var calls atomic.Int32
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeUsageResponse(w, 50)
	})
	targets := s.selector.reconcileTargets()
	require.Len(t, targets, 1)
	lease := requireLease(t, s.selector.pickFor(map[string]bool{}, ""))
	s.selector.recordFailure(lease)

	_, err := s.refreshUsageTarget(context.Background(), targets[0], nil)

	require.ErrorIs(t, err, errUsageObservationStale)
	assert.Equal(t, int32(0), calls.Load())
	assert.Equal(t, quotaUnknown, selectorQuota(t, s.selector, "acc"))
}

func TestUsageCacheIgnoresOldRevision(t *testing.T) {
	var calls atomic.Int32
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeUsageResponse(w, 40)
	})
	oldRevision := runtimeRevision(t, s.accounts, "acc")
	s.usageCache["acc"] = usageCacheEntry{
		usage:   &kiroUsage{Credit: &kiroCreditUsage{Remaining: 1}},
		fetched: time.Now(), revision: oldRevision,
	}
	require.NoError(t, discardChanged(s.accounts.SetOverageEnabledChanged("acc", true)))
	creds, ok := s.selector.byID("acc")
	require.True(t, ok)

	u, err := s.ensureUsageReadOnly(context.Background(), creds)
	require.NoError(t, err)
	assert.Equal(t, float64(40), u.Credit.Remaining)
	assert.Equal(t, int32(1), calls.Load())
	s.usageMu.Lock()
	cached := s.usageCache["acc"]
	s.usageMu.Unlock()
	assert.Equal(t, creds.revision, cached.revision)
}

func TestUsageInvalidationRejectsInFlightWriteAndJoin(t *testing.T) {
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
			writeUsageResponse(w, 10)
		case 2:
			close(secondStarted)
			writeUsageResponse(w, 60)
		default:
			writeUsageResponse(w, 60)
		}
	})
	unblockFirst := cleanupTestRelease(t, releaseFirst)

	firstErr := make(chan error, 1)
	go func() {
		_, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
		firstErr <- err
	}()
	<-firstStarted
	s.invalidateUsage("acc")
	secondErr := make(chan error, 1)
	go func() {
		_, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
		secondErr <- err
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("post-invalidation refresh joined stale in-flight fetch")
	}
	require.NoError(t, <-secondErr)
	unblockFirst()
	require.NoError(t, <-firstErr)

	assert.Equal(t, int32(3), calls.Load())
	s.usageMu.Lock()
	cached := s.usageCache["acc"]
	s.usageMu.Unlock()
	require.NotNil(t, cached.usage)
	assert.Equal(t, float64(60), cached.usage.Credit.Remaining)
}

func TestUsageCacheTimestampIsFetchCompletion(t *testing.T) {
	var completed atomic.Int64
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		writeUsageResponse(w, 50)
		completed.Store(time.Now().UnixNano())
	})
	_, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
	require.NoError(t, err)
	s.usageMu.Lock()
	fetched := s.usageCache["acc"].fetched
	s.usageMu.Unlock()
	assert.GreaterOrEqual(t, fetched.UnixNano(), completed.Load())
}

func TestModelsCacheIgnoresOldRevision(t *testing.T) {
	var calls atomic.Int32
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{{"modelId": "fresh-model"}}})
	})
	oldRevision := runtimeRevision(t, s.accounts, "acc")
	s.modelsCache["acc"] = modelsCacheEntry{
		models: []kiroModelInfo{{ModelID: "stale-model"}}, fetched: time.Now(), revision: oldRevision,
	}
	require.NoError(t, discardChanged(s.accounts.SetOverageEnabledChanged("acc", true)))
	creds, ok := s.selector.byID("acc")
	require.True(t, ok)

	models, err := s.ensureModels(context.Background(), creds)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "fresh-model", models[0].ModelID)
	assert.Equal(t, int32(1), calls.Load())
	s.modelsMu.Lock()
	cached := s.modelsCache["acc"]
	s.modelsMu.Unlock()
	assert.Equal(t, creds.revision, cached.revision)
}

func TestModelsInvalidationRejectsInFlightWrite(t *testing.T) {
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	s := newUsageBackedServer(t, true, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{{"modelId": "stale-model"}}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{{"modelId": "fresh-model"}}})
	})
	unblockFirst := cleanupTestRelease(t, releaseFirst)
	creds, ok := s.selector.byID("acc")
	require.True(t, ok)

	firstErr := make(chan error, 1)
	go func() {
		_, err := s.ensureModels(context.Background(), creds)
		firstErr <- err
	}()
	<-firstStarted
	s.invalidateModels("acc")
	models, err := s.ensureModels(context.Background(), creds)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "fresh-model", models[0].ModelID)
	unblockFirst()
	require.ErrorIs(t, <-firstErr, errModelResultStale)

	s.modelsMu.Lock()
	cached := s.modelsCache["acc"]
	s.modelsMu.Unlock()
	require.Len(t, cached.models, 1)
	assert.Equal(t, "fresh-model", cached.models[0].ModelID)
	assert.Equal(t, int32(2), calls.Load())
}

func TestWarmAccountRetriesModelsAfterProfileBackfill(t *testing.T) {
	var profileCalls atomic.Int32
	var modelCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Amz-Target") {
		case "AmazonCodeWhispererService.ListAvailableProfiles":
			profileCalls.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{
				"profiles": []map[string]any{{"arn": "arn:resolved"}},
			})
		case "KiroControlPlaneBearerService.ListAvailableModels":
			modelCalls.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{
				"models": []map[string]any{{"modelId": "claude-sonnet-5"}},
			})
		default:
			writeUsageResponse(w, 50)
		}
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acc", ClientID: "client", ClientSecret: "secret", RefreshToken: "refresh",
		AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		Region: "us-east-1", OverageEnabled: true, CreatedAt: "1",
	}))
	s := NewServer(&Config{}, client)
	s.setAccounts(store, client)

	s.warmAccountSync(context.Background(), "acc")

	rt, ok := store.Runtime("acc")
	require.True(t, ok)
	assert.Equal(t, "arn:resolved", rt.Account.ProfileArn)
	s.modelsMu.Lock()
	cached, cachedOK := s.modelsCache["acc"]
	s.modelsMu.Unlock()
	require.True(t, cachedOK)
	assert.Equal(t, rt.Revision, cached.revision)
	require.Len(t, cached.models, 1)
	assert.Equal(t, "claude-sonnet-5", cached.models[0].ModelID)
	assert.GreaterOrEqual(t, profileCalls.Load(), int32(1))
	assert.LessOrEqual(t, profileCalls.Load(), int32(2))
	assert.Equal(t, int32(2), modelCalls.Load(), "stale successful fetch should retry on the current revision")
}

func TestRefreshUsageCanceledContextDoesNotStartFetch(t *testing.T) {
	var calls atomic.Int32
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected usage fetch", http.StatusInternalServerError)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.refreshUsage(ctx, "acc", usageObservationAuthoritative)
	require.ErrorIs(t, err, context.Canceled)
	require.Never(t, func() bool { return calls.Load() != 0 }, 100*time.Millisecond, 5*time.Millisecond)
}

func TestWarmAllAccountsCanceledContextStartsNoRequests(t *testing.T) {
	var calls atomic.Int32
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected warmup request", http.StatusInternalServerError)
	})
	lifetime, cancel := context.WithCancel(context.Background())
	cancel()
	s.setWarmupContext(lifetime)

	s.warmAllAccounts()
	require.Never(t, func() bool { return calls.Load() != 0 }, 100*time.Millisecond, 5*time.Millisecond)
}

func TestWarmAccountBoundsAndCancelsModelFetch(t *testing.T) {
	type observation struct {
		deadline time.Time
		has      bool
	}
	started := make(chan observation, 1)
	done := make(chan error, 1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Amz-Target") != "KiroControlPlaneBearerService.ListAvailableModels" {
			return nil, context.Canceled
		}
		deadline, ok := r.Context().Deadline()
		started <- observation{deadline: deadline, has: ok}
		<-r.Context().Done()
		done <- r.Context().Err()
		return nil, r.Context().Err()
	})}
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acc", AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		ProfileArn: "arn:test", Region: "us-east-1", OverageEnabled: true, CreatedAt: "1",
	}))
	s := NewServer(&Config{}, client)
	s.setAccounts(store, client)
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.setWarmupContext(lifetime)

	s.warmAccount("acc")
	var observed observation
	select {
	case observed = <-started:
	case <-time.After(time.Second):
		t.Fatal("model warmup did not start")
	}
	require.True(t, observed.has, "model warmup request must carry a deadline")
	remaining := time.Until(observed.deadline)
	assert.Greater(t, remaining, time.Duration(0))
	assert.LessOrEqual(t, remaining, modelWarmupTimeout)

	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("model warmup did not stop with the service context")
	}
}

func TestWarmModelsStopsWhileJoiningExistingTokenRefresh(t *testing.T) {
	var tokenCalls atomic.Int32
	var modelCalls atomic.Int32
	tokenStarted := make(chan struct{})
	releaseToken := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseToken)
		}
	}()

	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if tokenCalls.Add(1) != 1 {
				http.Error(w, "unexpected second token refresh", http.StatusInternalServerError)
				return
			}
			close(tokenStarted)
			<-releaseToken
			writeJSON(w, http.StatusOK, map[string]any{
				"accessToken": "fresh-access", "refreshToken": "fresh-refresh",
				"tokenType": "Bearer", "expiresIn": 3600,
			})
		case r.Header.Get("X-Amz-Target") == "KiroControlPlaneBearerService.ListAvailableModels":
			modelCalls.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{
				"models": []map[string]any{{"modelId": "claude-sonnet-5"}},
			})
		default:
			writeUsageResponse(w, 50)
		}
	})
	require.NoError(t, s.accounts.UpdateTokens("acc", "", "", ""))

	ownerDone := make(chan error, 1)
	go func() {
		_, err := s.accounts.RefreshToken(context.Background(), s.kiro.client, "acc")
		ownerDone <- err
	}()
	select {
	case <-tokenStarted:
	case <-time.After(time.Second):
		t.Fatal("token refresh did not start")
	}

	lifetime, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	warmDone := make(chan struct{})
	go func() {
		s.warmModels(lifetime, "acc")
		close(warmDone)
	}()
	select {
	case <-warmDone:
	case <-time.After(time.Second):
		t.Fatal("model warmup stayed blocked on an existing token refresh")
	}
	assert.Equal(t, int32(1), tokenCalls.Load())
	assert.Equal(t, int32(0), modelCalls.Load(), "canceled warmup must not reach the model endpoint")

	close(releaseToken)
	released = true
	select {
	case err := <-ownerDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("token refresh owner did not finish after release")
	}
	got, ok := s.accounts.Get("acc")
	require.True(t, ok)
	assert.Equal(t, "fresh-access", got.AccessToken)
}

func TestOpenStreamVerifiesUnknownStrictAccount(t *testing.T) {
	for _, tc := range []struct {
		name          string
		remaining     float64
		missingCredit bool
		wantRuntime   int32
		wantNoError   bool
	}{
		{name: "positive usage admits", remaining: 50, wantRuntime: 1, wantNoError: true},
		{name: "zero usage stays blocked", remaining: 0, wantRuntime: 0, wantNoError: false},
		{name: "missing credit stays blocked", missingCredit: true, wantRuntime: 0, wantNoError: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usageCalls atomic.Int32
			var runtimeCalls atomic.Int32
			s := newUsageBackedServer(t, true, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/getUsageLimits" {
					usageCalls.Add(1)
					if tc.missingCredit {
						writeJSON(w, http.StatusOK, map[string]any{})
					} else {
						writeUsageResponse(w, tc.remaining)
					}
					return
				}
				runtimeCalls.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(contentFrame("ok"))
			})
			stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
			if tc.wantNoError {
				require.NoError(t, err)
				require.NotNil(t, stream)
				stream.Close()
			} else {
				require.Error(t, err)
			}
			assert.Equal(t, int32(1), usageCalls.Load())
			assert.Equal(t, tc.wantRuntime, runtimeCalls.Load())
		})
	}
}

func reqForModel(model string) *kiroRequest {
	return &kiroRequest{ConversationState: kiroConversationState{
		CurrentMessage: kiroMessage{UserInputMessage: &kiroUserInputMessage{ModelID: model}}}}
}

func effortOf(k *kiroRequest) string {
	oc, ok := k.AdditionalModelRequestFields["output_config"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := oc["effort"].(string)
	return s
}

func TestApplyModelRequestFields(t *testing.T) {
	s := testServerWithModels()
	ctx := context.Background()

	// effort unspecified -> default max; max_tokens unspecified -> ceiling.
	k := reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 0)
	assert.Equal(t, "max", effortOf(k), "default effort")
	assert.Equal(t, 128000, k.AdditionalModelRequestFields["max_tokens"], "default max_tokens")

	// caller values honored (in range).
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "low", 5000)
	assert.Equal(t, "low", effortOf(k))
	assert.Equal(t, 5000, k.AdditionalModelRequestFields["max_tokens"])

	// max_tokens below schema minimum clamps up.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 80)
	assert.Equal(t, 1024, k.AdditionalModelRequestFields["max_tokens"], "min clamp")

	// max_tokens above ceiling clamps down.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "", 999999)
	assert.Equal(t, 128000, k.AdditionalModelRequestFields["max_tokens"], "max clamp")

	// unsupported effort level clamps to the model's highest.
	k = reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(ctx, k, "ultra", 0)
	assert.Equal(t, "max", effortOf(k), "ultra should clamp to max")

	// model without schema -> untouched.
	k = reqForModel("claude-sonnet-4.5")
	s.applyModelRequestFields(ctx, k, "high", 1000)
	assert.Nil(t, k.AdditionalModelRequestFields, "sonnet-4.5 should be untouched")

	// "auto" -> untouched.
	k = reqForModel("auto")
	s.applyModelRequestFields(ctx, k, "high", 1000)
	assert.Nil(t, k.AdditionalModelRequestFields, "auto should be untouched")
}

// TestApplyFieldsForModel exercises the per-account free function directly (no
// Server / anyModels lookup), the path openStream uses after resolveModel.
func TestApplyFieldsForModel(t *testing.T) {
	// defaults: effort tops out, max_tokens at the ceiling.
	k := reqForModel("claude-opus-4.8")
	applyFieldsForModel(k, testOpusModel(), "", 0)
	assert.Equal(t, "max", effortOf(k))
	assert.Equal(t, 128000, k.AdditionalModelRequestFields["max_tokens"])

	// caller values honored within range.
	k = reqForModel("claude-opus-4.8")
	applyFieldsForModel(k, testOpusModel(), "low", 5000)
	assert.Equal(t, "low", effortOf(k))
	assert.Equal(t, 5000, k.AdditionalModelRequestFields["max_tokens"])

	// below the schema minimum clamps up.
	k = reqForModel("claude-opus-4.8")
	applyFieldsForModel(k, testOpusModel(), "", 80)
	assert.Equal(t, 1024, k.AdditionalModelRequestFields["max_tokens"])

	// model without a schema is left untouched.
	k = reqForModel("claude-sonnet-4.5")
	applyFieldsForModel(k, testSonnet45Model(), "high", 1000)
	assert.Nil(t, k.AdditionalModelRequestFields, "model without schema should be untouched")
}

func TestIsInvalidModelError(t *testing.T) {
	// Machine reason code wins (pre-stream path sets ReasonCode).
	assert.True(t, isInvalidModelError(&kiroHTTPError{Status: 400, ReasonCode: "INVALID_MODEL_ID"}))
	// Body-text fallback when the reason code is absent (the plain-HTTP path).
	assert.True(t, isInvalidModelError(&kiroHTTPError{
		Status: 400, Body: `{"message":"Invalid model ID. Please select a different model.","reason":"X"}`}))

	// Wrong status is never a model error.
	assert.False(t, isInvalidModelError(&kiroHTTPError{Status: 500, ReasonCode: "INVALID_MODEL_ID"}))
	// A generic 400 without the phrase is not a model error.
	assert.False(t, isInvalidModelError(&kiroHTTPError{Status: 400, Body: `{"message":"bad input"}`}))
	// Crucially, a 400 that merely contains "invalid model" (not "invalid model id")
	// must NOT be misclassified and turned into failover.
	assert.False(t, isInvalidModelError(&kiroHTTPError{Status: 400, Body: `{"message":"invalid model parameter"}`}))
	// Sibling validation errors stay distinct.
	assert.False(t, isInvalidModelError(&kiroHTTPError{Status: 400, ReasonCode: "PROMPT_TOO_LONG"}))
	assert.False(t, isInvalidModelError(&kiroHTTPError{Status: 400, ReasonCode: "THINKING_SIGNATURE_INVALID"}))
}

func TestModelInfoJSON(t *testing.T) {
	info := modelInfoJSON(testOpusModel(), "2026-01-01T00:00:00Z")
	assert.Equal(t, "model", info["type"])
	assert.Equal(t, "claude-opus-4.8", info["id"])
	assert.Equal(t, 1000000, info["max_input_tokens"])
	assert.Equal(t, 128000, info["max_tokens"])
	assert.Equal(t, 1.5, info["rate_multiplier"])
	assert.Equal(t, "credit", info["rate_unit"])
	assert.Equal(t, "Most powerful model for complex tasks.", info["description"])
	assert.Equal(t, "ACTIVE", info["status"])
	assert.Equal(t, []string{"text", "image"}, info["supported_input_types"])
	pc := info["prompt_caching"].(map[string]any)
	assert.Equal(t, true, pc["supported"])

	caps := info["capabilities"].(map[string]any)["effort"].(map[string]any)
	assert.Equal(t, true, caps["supported"])
	assert.Equal(t, true, caps["max"])
	assert.Equal(t, true, caps["xhigh"])

	// sonnet-4.5: no effort support.
	sonnetInfo := modelInfoJSON(testSonnet45Model(), "x")
	caps = sonnetInfo["capabilities"].(map[string]any)["effort"].(map[string]any)
	assert.Equal(t, false, caps["supported"])

	// sonnet-4.5: no rate multiplier → keys omitted.
	_, hasRate := sonnetInfo["rate_multiplier"]
	assert.False(t, hasRate)
	_, hasUnit := sonnetInfo["rate_unit"]
	assert.False(t, hasUnit)

	// sonnet-4.5: no description/status/inputTypes/caching → keys omitted.
	for _, k := range []string{"description", "status", "supported_input_types", "prompt_caching"} {
		_, ok := sonnetInfo[k]
		assert.False(t, ok, "%s should be absent for sonnet", k)
	}
}

func TestMapUpstreamError(t *testing.T) {
	cases := []struct {
		status  int
		wantSt  int
		wantTyp string
	}{
		{401, 401, "authentication_error"},
		{403, 403, "permission_error"},
		{429, 429, "rate_limit_error"},
		{400, 400, "invalid_request_error"},
		{http.StatusPaymentRequired, http.StatusPaymentRequired, "api_error"},
		{http.StatusLocked, http.StatusLocked, "api_error"},
		{500, http.StatusBadGateway, "api_error"},
	}
	for _, c := range cases {
		st, typ := mapUpstreamError(&kiroHTTPError{Status: c.status})
		assert.Equalf(t, c.wantSt, st, "status %d", c.status)
		assert.Equalf(t, c.wantTyp, typ, "status %d", c.status)
	}
	// non-kiroHTTPError -> 502 api_error
	st, typ := mapUpstreamError(context.Canceled)
	assert.Equal(t, http.StatusBadGateway, st)
	assert.Equal(t, "api_error", typ)
}

func TestAuthorized(t *testing.T) {
	// open (no key) -> always authorized
	open := &Server{cfg: &Config{}}
	assert.True(t, open.authorized(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)),
		"open server should authorize")

	s := &Server{cfg: &Config{APIKey: "secret"}}
	mk := func(set func(*http.Request)) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		set(r)
		return r
	}
	assert.True(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("x-api-key", "secret") })),
		"matching x-api-key should authorize")
	assert.False(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("x-api-key", "wrong") })),
		"wrong x-api-key should reject")
	assert.True(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") })),
		"matching bearer should authorize")
	assert.True(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("Authorization", "bearer secret") })),
		"scheme is case-insensitive")
	assert.False(t, s.authorized(mk(func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") })),
		"wrong bearer should reject")
	assert.False(t, s.authorized(mk(func(r *http.Request) {})),
		"missing key should reject")
}

func TestApplyModelRequestFieldsMinimize(t *testing.T) {
	s := testServerWithModels()
	// thinking disabled -> minimize sentinel -> the model's lowest effort level.
	k := reqForModel("claude-opus-4.8")
	s.applyModelRequestFields(context.Background(), k, effortMinimize, 0)
	assert.Equal(t, "low", effortOf(k), "minimize effort")
}

func TestWithReasoningRetry(t *testing.T) {
	sigErr := &kiroHTTPError{Status: 400, Body: `{"reason":"THINKING_SIGNATURE_INVALID"}`}

	// history carrying reasoning on an assistant turn.
	withReasoning := func() *kiroRequest {
		return &kiroRequest{ConversationState: kiroConversationState{History: []kiroMessage{
			{UserInputMessage: &kiroUserInputMessage{Content: "hi"}},
			{AssistantResponseMessage: &kiroAssistantMessage{Content: "a",
				ReasoningContent: &kiroReasoningContent{ReasoningText: &kiroReasoningText{Text: "t", Signature: "s"}}}},
		}}}
	}
	hasReasoning := func(k *kiroRequest) bool {
		for _, m := range k.ConversationState.History {
			if m.AssistantResponseMessage != nil && m.AssistantResponseMessage.ReasoningContent != nil {
				return true
			}
		}
		return false
	}

	// 1. first send succeeds -> no retry, stream returned as-is.
	{
		want := &kiroStream{}
		calls := 0
		got, err := withReasoningRetry(withReasoning(), func(*kiroRequest) (*kiroStream, error) {
			calls++
			return want, nil
		})
		require.NoError(t, err)
		assert.Same(t, want, got)
		assert.Equal(t, 1, calls)
	}

	// 2. signature error + reasoning present -> retry once, history stripped, retry result returned.
	{
		want := &kiroStream{}
		calls := 0
		var strippedOnRetry bool
		k := withReasoning()
		got, err := withReasoningRetry(k, func(kk *kiroRequest) (*kiroStream, error) {
			calls++
			if calls == 1 {
				return nil, sigErr
			}
			strippedOnRetry = !hasReasoning(kk) // history must be stripped before the retry send
			return want, nil
		})
		require.NoError(t, err)
		assert.Same(t, want, got)
		assert.Equal(t, 2, calls)
		assert.True(t, strippedOnRetry, "history should be stripped before retry")
	}

	// 3. signature error but nothing to strip -> no retry, original error surfaces.
	{
		calls := 0
		bare := &kiroRequest{}
		_, err := withReasoningRetry(bare, func(*kiroRequest) (*kiroStream, error) {
			calls++
			return nil, sigErr
		})
		assert.Same(t, sigErr, err)
		assert.Equal(t, 1, calls)
	}

	// 4. unrelated error -> no retry, original error surfaces.
	{
		calls := 0
		other := &kiroHTTPError{Status: 400, Body: `{"reason":"PROMPT_TOO_LONG"}`}
		_, err := withReasoningRetry(withReasoning(), func(*kiroRequest) (*kiroStream, error) {
			calls++
			return nil, other
		})
		assert.Same(t, other, err)
		assert.Equal(t, 1, calls)
	}
}

func TestIsThinkingSignatureError(t *testing.T) {
	assert.True(t, isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `{"message":"bad","reason":"THINKING_SIGNATURE_INVALID"}`}),
		"reason code should match")
	// message sniff fallback when no machine reason present
	assert.True(t, isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `The thinking signature is invalid`}),
		"message sniff should match")
	assert.False(t, isThinkingSignatureError(&kiroHTTPError{Status: 400, Body: `{"reason":"PROMPT_TOO_LONG"}`}),
		"unrelated reason should not match")
	assert.False(t, isThinkingSignatureError(&kiroHTTPError{Status: 500, Body: `thinking signature`}),
		"non-400 should not match")
	assert.False(t, isThinkingSignatureError(context.Canceled),
		"non-http error should not match")
}

func TestIsPromptTooLongError(t *testing.T) {
	assert.True(t, isPromptTooLongError(&kiroHTTPError{
		Status: http.StatusBadRequest,
		Body:   `{"message":"too long","reason":"PROMPT_TOO_LONG"}`,
	}), "HTTP body reason should match")
	assert.True(t, isPromptTooLongError(&kiroHTTPError{
		Status:     http.StatusBadRequest,
		Body:       "prompt exceeds context",
		ReasonCode: "PROMPT_TOO_LONG",
	}), "explicit event reason should match")
	assert.False(t, isPromptTooLongError(&kiroHTTPError{
		Status: http.StatusBadRequest,
		Body:   `{"reason":"OTHER_VALIDATION_ERROR"}`,
	}), "other validation errors must not trim history")
	assert.False(t, isPromptTooLongError(&kiroHTTPError{
		Status: http.StatusBadRequest,
		Body:   "prompt too long",
	}), "human-readable text alone must not match")
	assert.False(t, isPromptTooLongError(&kiroHTTPError{
		Status:     http.StatusBadGateway,
		ReasonCode: "PROMPT_TOO_LONG",
	}), "only request-level 400 errors should match")
	assert.False(t, isPromptTooLongError(context.Canceled))
}

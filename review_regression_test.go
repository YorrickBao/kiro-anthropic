package main

import (
	"context"
	"encoding/json"
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

func protocolEventFrame(kind, payload string) []byte {
	headers := append(
		esStringHeader(":message-type", "event"),
		esStringHeader(":event-type", kind)...,
	)
	return esFrame(headers, []byte(payload))
}

func TestOpenStreamScansIgnoredFramesBeforeFailure(t *testing.T) {
	rt := newFakeRuntime(t)
	ignored := protocolEventFrame("contextUsageEvent", `{}`)
	rt.bodies["Bearer bad"] = append(ignored, exceptionFrame("InternalServerException", "failed")...)
	rt.bodies["Bearer good"] = contentFrame("ok")
	s := serverWithPool(t, rt, "bad", "good")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()
	ev, err := stream.Recv()
	require.NoError(t, err)
	assert.Equal(t, "ok", ev.Text)
	assert.Equal(t, []string{"Bearer bad", "Bearer good"}, rt.seen)
}

func TestOpenStreamScansSuppressedReasoningBeforeFailure(t *testing.T) {
	rt := newFakeRuntime(t)
	model := kiroModelInfo{ModelID: "claude-sonnet-5"}
	rt.modelsFor["Bearer bad"] = []kiroModelInfo{model}
	rt.modelsFor["Bearer good"] = []kiroModelInfo{model}
	reasoning := protocolEventFrame("reasoningContentEvent", `{"text":"hidden"}`)
	rt.bodies["Bearer bad"] = append(reasoning, exceptionFrame("InternalServerException", "failed")...)
	rt.bodies["Bearer good"] = contentFrame("ok")
	s := serverWithPool(t, rt, "bad", "good")
	areq := msgAreq("claude-sonnet-5")
	areq.Thinking = &anthropicThinking{Type: "disabled"}

	stream, err := s.openStream(context.Background(), &kiroRequest{}, areq)
	require.NoError(t, err)
	defer stream.Close()
	assert.Equal(t, "claude-sonnet-5", stream.modelID)
	assert.Equal(t, []string{"Bearer bad", "Bearer good"}, rt.sendTokens)
}

func TestOpenStreamTreatsEmptyEOFAsPrecommitFailure(t *testing.T) {
	rt := newFakeRuntime(t)
	rt.bodies["Bearer empty"] = []byte{}
	rt.bodies["Bearer good"] = contentFrame("ok")
	s := serverWithPool(t, rt, "empty", "good")

	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()
	assert.Equal(t, []string{"Bearer empty", "Bearer good"}, rt.seen)
}

func TestOpenStreamFirstFrameBudgetHandsPendingReadToRecv(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
			_, _ = w.Write(contentFrame("delayed"))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	unblock := cleanupTestRelease(t, release)
	rt := &fakeRuntime{
		srv: upstream, badTokens: map[string]bool{}, unauthorizedTokens: map[string]bool{},
		bodies: map[string][]byte{}, modelsFor: map[string][]kiroModelInfo{},
		modelsError: map[string]bool{}, invalidModel: map[string]bool{},
	}
	s := serverWithPool(t, rt, "acc")

	start := time.Now()
	stream, err := s.openStream(context.Background(), &kiroRequest{}, nil)
	require.NoError(t, err)
	defer stream.Close()
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, firstFrameProbeTimeout-100*time.Millisecond)
	assert.Less(t, elapsed, firstFrameProbeTimeout+time.Second)

	unblock()
	ev, err := stream.Recv()
	require.NoError(t, err)
	assert.Equal(t, "delayed", ev.Text)
}

func TestKiroEventFailureNormalizesAWSAliases(t *testing.T) {
	cases := []struct {
		kind     string
		status   int
		depleted bool
	}{
		{kind: "validationexception", status: http.StatusBadRequest},
		{kind: "ThrottlingException", status: http.StatusTooManyRequests},
		{kind: "THROTTLINGERROR", status: http.StatusTooManyRequests},
		{kind: "ServiceQuotaExceededException", status: http.StatusPaymentRequired, depleted: true},
		{kind: "SERVICEQUOTAEXCEEDEDERROR", status: http.StatusPaymentRequired, depleted: true},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			err := kiroEventFailure(&kiroEvent{Kind: evError, ErrKind: tc.kind, ErrMsg: "failed"})
			var upstream *kiroHTTPError
			require.ErrorAs(t, err, &upstream)
			assert.Equal(t, tc.status, upstream.Status)
			assert.Equal(t, tc.depleted, isAccountDepleted(err))
		})
	}
}

func TestMessagesReportConcreteResolvedModel(t *testing.T) {
	for _, streamResponse := range []bool{false, true} {
		name := "aggregate"
		if streamResponse {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			rt := newFakeRuntime(t)
			rt.modelsFor["Bearer acc"] = []kiroModelInfo{{ModelID: "claude-opus-4.8", Default: true}}
			s := serverWithPool(t, rt, "acc")
			body := `{"model":"auto","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			if streamResponse {
				body = `{"model":"auto","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			rec := httptest.NewRecorder()

			s.Handler().ServeHTTP(rec, req)

			assert.Equal(t, http.StatusOK, rec.Code)
			if streamResponse {
				assert.Contains(t, rec.Body.String(), `"model":"claude-opus-4.8"`)
				return
			}
			var response anthropicResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			assert.Equal(t, "claude-opus-4.8", response.Model)
		})
	}
}

func TestOpenStreamReselectsReplacementAfterStaleModelFetch(t *testing.T) {
	rt := newFakeRuntime(t)
	model := kiroModelInfo{ModelID: "claude-sonnet-5"}
	rt.modelsFor["Bearer old"] = []kiroModelInfo{model}
	rt.modelsFor["Bearer replacement"] = []kiroModelInfo{model}
	started := make(chan struct{})
	release := make(chan struct{})
	unblock := cleanupTestRelease(t, release)
	var once sync.Once
	rt.modelsHook = func(token string) {
		if token == "Bearer old" {
			once.Do(func() { close(started) })
			<-release
		}
	}
	s := serverWithPool(t, rt, "old")
	done := make(chan error, 1)
	go func() {
		stream, err := s.openStream(context.Background(), &kiroRequest{}, msgAreq("claude-sonnet-5"))
		if stream != nil {
			stream.Close()
		}
		done <- err
	}()
	<-started
	fresh, ok := s.accounts.Get("old")
	require.True(t, ok)
	fresh.AccessToken = "replacement"
	require.NoError(t, s.accounts.ReplaceCredentials("old", &fresh))
	unblock()
	require.NoError(t, <-done)
	assert.Equal(t, []string{"Bearer replacement"}, rt.sendTokens)
}

func TestEnsureModelsCoalescesConcurrentMisses(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != "KiroControlPlaneBearerService.ListAvailableModels" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{{"modelId": "claude-sonnet-5"}}})
	})
	unblock := cleanupTestRelease(t, release)
	creds, ok := s.selector.byID("acc")
	require.True(t, ok)

	const waiters = 12
	errs := make(chan error, waiters)
	for range waiters {
		go func() {
			_, err := s.ensureModels(context.Background(), creds)
			errs <- err
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	unblock()
	for range waiters {
		require.NoError(t, <-errs)
	}
	assert.Equal(t, int32(1), calls.Load())
}

func TestWarmAllAccountsHasFixedUpstreamConcurrency(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	var calls atomic.Int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		if r.Header.Get("X-Amz-Target") == "KiroControlPlaneBearerService.ListAvailableModels" {
			writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{{"modelId": "claude-sonnet-5"}}})
			return
		}
		writeUsageResponse(w, 50)
	}))
	t.Cleanup(upstream.Close)
	unblock := cleanupTestRelease(t, release)
	target, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &rewriteTransport{host: target.Host, scheme: target.Scheme}}
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		require.NoError(t, store.Add(&StoredAccount{
			ID: id, AccessToken: id, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			ProfileArn: "arn:" + id, Region: "us-east-1", OverageEnabled: true, CreatedAt: id,
		}))
	}
	s := NewServer(&Config{}, client)
	s.setAccounts(store, client)
	s.warmAllAccounts()
	require.Eventually(t, func() bool {
		return peak.Load() == int32(2*warmupConcurrency)
	}, time.Second, 10*time.Millisecond)
	assert.LessOrEqual(t, peak.Load(), int32(2*warmupConcurrency))
	unblock()
	require.Eventually(t, func() bool {
		return calls.Load() == 24 && active.Load() == 0
	}, 2*time.Second, 10*time.Millisecond)
}

func TestUsageOlderGenerationCannotOverwriteNewerCache(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			writeUsageResponse(w, 10)
			return
		}
		writeUsageResponse(w, 60)
	})
	unblock := cleanupTestRelease(t, release)
	creds, ok := s.selector.byID("acc")
	require.True(t, ok)
	oldResult := make(chan *kiroUsage, 1)
	oldErr := make(chan error, 1)
	go func() {
		u, err := s.ensureUsageReadOnly(context.Background(), creds)
		oldResult <- u
		oldErr <- err
	}()
	<-started
	lease := requireLease(t, s.selector.pickFor(map[string]bool{}, ""))
	s.selector.recordFailure(lease)

	newer, err := s.ensureUsageReadOnly(context.Background(), creds)
	require.NoError(t, err)
	require.NotNil(t, newer.Credit)
	assert.Equal(t, float64(60), newer.Credit.Remaining)
	unblock()
	require.NoError(t, <-oldErr)
	old := <-oldResult
	require.NotNil(t, old.Credit)
	assert.Equal(t, float64(10), old.Credit.Remaining)

	s.usageMu.Lock()
	cached := s.usageCache["acc"]
	s.usageMu.Unlock()
	require.NotNil(t, cached.usage)
	assert.Equal(t, float64(60), cached.usage.Credit.Remaining)
	assert.Equal(t, int32(2), calls.Load())
}

func TestProbeAndAuthoritativeWaitersPreserveAuthoritativeSemantics(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"overageConfiguration": map[string]any{"overageStatus": "ENABLED"},
			"usageBreakdownList": []map[string]any{{
				"resourceType": "CREDIT", "currentUsageWithPrecision": 100,
				"usageLimitWithPrecision": 100, "overageCapWithPrecision": 100,
			}},
		})
	})
	unblock := cleanupTestRelease(t, release)
	probeErr := make(chan error, 1)
	go func() {
		_, err := s.refreshUsage(context.Background(), "acc", usageObservationProbe)
		probeErr <- err
	}()
	<-started
	authoritativeErr := make(chan error, 1)
	go func() {
		_, err := s.refreshUsage(context.Background(), "acc", usageObservationAuthoritative)
		authoritativeErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	unblock()

	require.NoError(t, <-authoritativeErr)
	if err := <-probeErr; err != nil {
		assert.ErrorIs(t, err, errUsageObservationStale)
	}
	assert.Equal(t, quotaOverage, selectorQuota(t, s.selector, "acc"))
	assert.NotNil(t, s.selector.pickFor(map[string]bool{}, "").lease)
	assert.GreaterOrEqual(t, calls.Load(), int32(1))
	assert.LessOrEqual(t, calls.Load(), int32(2))
}

func TestAggregateExcludesAllDepletedStates(t *testing.T) {
	s := testServerWithModels()
	revision := runtimeRevision(t, s.accounts, "acc")
	s.usageCache["acc"] = usageCacheEntry{
		usage:   &kiroUsage{Credit: &kiroCreditUsage{Remaining: 50}},
		fetched: time.Now(), revision: revision,
	}
	assert.True(t, applyFreshUsage(t, s.selector, "acc", &kiroUsage{
		OverageStatus: "DISABLED",
		Credit:        &kiroCreditUsage{Remaining: 0},
	}, usageObservationAuthoritative))
	assert.False(t, s.selector.isReactivelyDepleted("acc", revision))

	aggregate := s.aggregateModelUsage(context.Background(), "claude-opus-4.8")
	assert.Empty(t, aggregate.Accounts)
	assert.Equal(t, 1, aggregate.Excluded)
	assert.Zero(t, aggregate.Totals.Accounts)
}

func TestNoOpOverageHandlerPreservesCachesAndStartsNoWarmup(t *testing.T) {
	var calls atomic.Int32
	s := newUsageBackedServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})
	revision := runtimeRevision(t, s.accounts, "acc")
	s.usageCache["acc"] = usageCacheEntry{
		usage:   &kiroUsage{Credit: &kiroCreditUsage{Remaining: 50}},
		fetched: time.Now(), revision: revision,
	}
	beforeEpoch := s.usageEpoch["acc"]
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/overage", strings.NewReader(`{"id":"acc","overageEnabled":true}`))
	rec := httptest.NewRecorder()

	s.handleAccountOverage(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, revision, runtimeRevision(t, s.accounts, "acc"))
	s.usageMu.Lock()
	_, cached := s.usageCache["acc"]
	afterEpoch := s.usageEpoch["acc"]
	s.usageMu.Unlock()
	assert.True(t, cached)
	assert.Equal(t, beforeEpoch, afterEpoch)
	require.Never(t, func() bool { return calls.Load() != 0 }, 100*time.Millisecond, 10*time.Millisecond)
}

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type conversationTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *conversationTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *conversationTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestConversationRegistry(clock *conversationTestClock, maxEntries int) (*conversationRegistry, *atomic.Int64) {
	var generated atomic.Int64
	return &conversationRegistry{
		ttl:           defaultConversationTTL,
		sweepInterval: time.Millisecond,
		maxEntries:    maxEntries,
		now:           clock.Now,
		newID: func() string {
			return fmt.Sprintf("conversation-%d", generated.Add(1))
		},
	}, &generated
}

func conversationRegistrySize(r *conversationRegistry) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func TestConversationRegistryDefaultIDIsUUID(t *testing.T) {
	var registry conversationRegistry
	id, release, err := registry.Acquire("session")
	require.NoError(t, err)
	t.Cleanup(release)
	_, err = uuid.Parse(id)
	require.NoError(t, err)
}

func TestConversationRegistryKeepsSessionsStableAndIsolated(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, generated := newTestConversationRegistry(clock, 8)

	first, releaseFirst, err := registry.Acquire("session-a")
	require.NoError(t, err)
	releaseFirst()
	second, releaseSecond, err := registry.Acquire("session-a")
	require.NoError(t, err)
	releaseSecond()
	other, releaseOther, err := registry.Acquire("session-b")
	require.NoError(t, err)
	releaseOther()

	assert.Equal(t, first, second)
	assert.NotEqual(t, first, other)
	assert.Equal(t, int64(2), generated.Load())
}

func TestConversationRegistryConcurrentFirstAcquireCreatesOneID(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, generated := newTestConversationRegistry(clock, 8)

	const callers = 64
	start := make(chan struct{})
	ids := make(chan string, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			id, release, err := registry.Acquire("shared-session")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			ids <- id
			release()
		}()
	}
	close(start)
	wg.Wait()
	close(ids)

	var want string
	for id := range ids {
		if want == "" {
			want = id
		}
		assert.Equal(t, want, id)
	}
	assert.Equal(t, int64(1), generated.Load())
}

func TestConversationRegistryExpiresAtInactivityBoundary(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, generated := newTestConversationRegistry(clock, 8)

	first, release, err := registry.Acquire("session")
	require.NoError(t, err)
	release()

	clock.Advance(defaultConversationTTL - time.Nanosecond)
	beforeBoundary, release, err := registry.Acquire("session")
	require.NoError(t, err)
	release()
	assert.Equal(t, first, beforeBoundary)

	clock.Advance(defaultConversationTTL)
	atBoundary, release, err := registry.Acquire("session")
	require.NoError(t, err)
	release()
	assert.NotEqual(t, first, atBoundary)

	clock.Advance(defaultConversationTTL + time.Nanosecond)
	afterBoundary, release, err := registry.Acquire("session")
	require.NoError(t, err)
	release()
	assert.NotEqual(t, atBoundary, afterBoundary)
	assert.Equal(t, int64(3), generated.Load())
}

func TestConversationRegistryAcquireExistingPinsWithoutCreating(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, generated := newTestConversationRegistry(clock, 8)

	_, release, ok := registry.AcquireExisting("unseen-session")
	assert.False(t, ok)
	assert.Nil(t, release)
	assert.Zero(t, conversationRegistrySize(registry))
	assert.Zero(t, generated.Load())

	first, release, err := registry.Acquire("existing-session")
	require.NoError(t, err)
	release()
	clock.Advance(defaultConversationTTL - time.Second)

	pinned, release, ok := registry.AcquireExisting("existing-session")
	require.True(t, ok)
	assert.Equal(t, first, pinned)
	clock.Advance(defaultConversationTTL)
	registry.Sweep(clock.Now())
	assert.Equal(t, 1, conversationRegistrySize(registry))
	release()
	assert.Equal(t, int64(1), generated.Load())
}

func TestConversationRegistryPinsActiveRequests(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, generated := newTestConversationRegistry(clock, 8)

	first, releaseFirst, err := registry.Acquire("session")
	require.NoError(t, err)
	clock.Advance(2 * defaultConversationTTL)
	registry.Sweep(clock.Now())
	assert.Equal(t, 1, conversationRegistrySize(registry))

	second, releaseSecond, err := registry.Acquire("session")
	require.NoError(t, err)
	assert.Equal(t, first, second)
	releaseSecond()
	releaseFirst()
	assert.Equal(t, int64(1), generated.Load())
}

func TestConversationRegistryReleaseIsIdempotent(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, _ := newTestConversationRegistry(clock, 8)

	_, release, err := registry.Acquire("session")
	require.NoError(t, err)
	release()
	release()
	clock.Advance(defaultConversationTTL)
	registry.Sweep(clock.Now())
	assert.Zero(t, conversationRegistrySize(registry))
}

func TestConversationRegistryCapacityPreservesExistingSessions(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, generated := newTestConversationRegistry(clock, 1)

	existing, release, err := registry.Acquire("existing")
	require.NoError(t, err)
	release()

	_, rejectedRelease, err := registry.Acquire("new")
	require.ErrorIs(t, err, errConversationRegistryFull)
	assert.Nil(t, rejectedRelease)

	again, release, err := registry.Acquire("existing")
	require.NoError(t, err)
	assert.Equal(t, existing, again)
	release()

	clock.Advance(defaultConversationTTL)
	replacement, release, err := registry.Acquire("new")
	require.NoError(t, err)
	release()
	assert.NotEqual(t, existing, replacement)
	assert.Equal(t, int64(2), generated.Load())
}

func TestConversationRegistryRunSweepsAndStops(t *testing.T) {
	clock := &conversationTestClock{now: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	registry, _ := newTestConversationRegistry(clock, 8)

	_, release, err := registry.Acquire("session")
	require.NoError(t, err)
	release()
	clock.Advance(defaultConversationTTL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		registry.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return conversationRegistrySize(registry) == 0
	}, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registry cleaner did not stop after context cancellation")
	}
}

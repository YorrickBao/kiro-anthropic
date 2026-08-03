package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultConversationTTL           = 24 * time.Hour
	defaultConversationSweepInterval = time.Hour
	maxConversationSessions          = 65536
)

var errConversationRegistryFull = errors.New("conversation session registry is full")

type conversationEntry struct {
	conversationID string
	lastActivity   time.Time
	inFlight       int
}

// conversationRegistry keeps Claude Code sessions bound to one process-local
// Kiro conversation UUID. It stores only a digest of the session header and is
// independent of account-store and selector locks.
type conversationRegistry struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]*conversationEntry

	ttl           time.Duration
	sweepInterval time.Duration
	maxEntries    int
	now           func() time.Time
	newID         func() string
}

func (r *conversationRegistry) initLocked() {
	if r.entries == nil {
		r.entries = make(map[[sha256.Size]byte]*conversationEntry)
	}
	if r.ttl <= 0 {
		r.ttl = defaultConversationTTL
	}
	if r.sweepInterval <= 0 {
		r.sweepInterval = defaultConversationSweepInterval
	}
	if r.maxEntries <= 0 {
		r.maxEntries = maxConversationSessions
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.newID == nil {
		r.newID = uuid.NewString
	}
}

// AcquireExisting pins and returns a live mapping without creating one. Callers
// can use it at request entry so an existing conversation cannot expire during
// body parsing or image preparation, while malformed requests for unseen
// sessions still do not consume registry capacity.
func (r *conversationRegistry) AcquireExisting(sessionID string) (string, func(), bool) {
	key := sha256.Sum256([]byte(sessionID))

	r.mu.Lock()
	r.initLocked()
	conversationID, release, ok := r.acquireExistingLocked(key, r.now())
	r.mu.Unlock()
	return conversationID, release, ok
}

// Acquire returns the stable conversation UUID for sessionID and pins its entry
// until the returned release function runs. release is safe to call more than
// once. Callers must pass an already-normalized, valid session ID.
func (r *conversationRegistry) Acquire(sessionID string) (string, func(), error) {
	key := sha256.Sum256([]byte(sessionID))

	r.mu.Lock()
	r.initLocked()
	now := r.now()
	if conversationID, release, ok := r.acquireExistingLocked(key, now); ok {
		r.mu.Unlock()
		return conversationID, release, nil
	}

	if len(r.entries) >= r.maxEntries {
		r.sweepLocked(now)
		if len(r.entries) >= r.maxEntries {
			r.mu.Unlock()
			return "", nil, errConversationRegistryFull
		}
	}

	entry := &conversationEntry{
		conversationID: r.newID(),
		lastActivity:   now,
		inFlight:       1,
	}
	r.entries[key] = entry
	conversationID := entry.conversationID
	release := r.releaseFunc(key, entry)
	r.mu.Unlock()
	return conversationID, release, nil
}

func (r *conversationRegistry) acquireExistingLocked(key [sha256.Size]byte, now time.Time) (string, func(), bool) {
	entry, ok := r.entries[key]
	if !ok {
		return "", nil, false
	}
	if entry.inFlight == 0 && r.expiredLocked(entry, now) {
		delete(r.entries, key)
		return "", nil, false
	}
	entry.lastActivity = now
	entry.inFlight++
	return entry.conversationID, r.releaseFunc(key, entry), true
}

func (r *conversationRegistry) releaseFunc(key [sha256.Size]byte, entry *conversationEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.initLocked()
			if current := r.entries[key]; current == entry {
				if current.inFlight > 0 {
					current.inFlight--
				}
				current.lastActivity = r.now()
			}
		})
	}
}

func (r *conversationRegistry) expiredLocked(entry *conversationEntry, now time.Time) bool {
	return now.Sub(entry.lastActivity) >= r.ttl
}

func (r *conversationRegistry) sweepLocked(now time.Time) {
	for key, entry := range r.entries {
		if entry.inFlight == 0 && r.expiredLocked(entry, now) {
			delete(r.entries, key)
		}
	}
}

// Sweep removes idle entries whose inactivity has reached the configured TTL.
func (r *conversationRegistry) Sweep(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	r.sweepLocked(now)
}

// Run periodically removes expired entries until ctx is canceled.
func (r *conversationRegistry) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.Lock()
	r.initLocked()
	interval := r.sweepInterval
	r.mu.Unlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.mu.Lock()
			r.initLocked()
			r.sweepLocked(r.now())
			r.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

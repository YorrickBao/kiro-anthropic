package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Per-request credentials and multi-account selection.
//
// KiroClient.Send and the control-plane calls are written against the
// kiroCredentials interface so they act as a specific account (accountCreds),
// chosen round-robin by accountSelector.
// ---------------------------------------------------------------------------

// kiroCredentials supplies the bearer token, region and profileArn for one
// Kiro call, and can refresh its token on a 401/403.
type kiroCredentials interface {
	// accessToken returns a currently-valid bearer token, refreshing if needed.
	accessToken(ctx context.Context) (string, error)
	// apiRegion is the region hosting the Kiro runtime/management endpoints.
	apiRegion() string
	// profileArn resolves the CodeWhisperer profileArn for the request.
	profileArn(ctx context.Context) (string, error)
	// machineID returns a stable per-account fingerprint for the User-Agent,
	// so pooled accounts look like distinct Kiro IDE installs rather than one
	// host running many sessions. Empty means "no fingerprint".
	machineID() string
	// refresh unconditionally refreshes the token (used to recover from 401/403).
	refresh(ctx context.Context) error
}

// accountCreds serves a request from one stored (self-managed) account. Token
// refresh uses the account's own client registration and is written back to the
// account store; it never touches Kiro's on-disk cache.
type accountCreds struct {
	store      *AccountStore
	client     *http.Client
	id         string
	revision   uint64
	credential uint64
	acct       StoredAccount
}

func (c *accountCreds) apiRegion() string {
	if c.acct.Region != "" {
		return c.acct.Region
	}
	return "us-east-1"
}

// machineID returns a stable fingerprint for this account, embedded in the
// User-Agent so each pooled account looks like its own Kiro IDE install.
func (c *accountCreds) machineID() string { return machineIDFor(c.id) }

func (c *accountCreds) profileArn(ctx context.Context) (string, error) {
	if c.acct.ProfileArn != "" {
		return c.acct.ProfileArn, nil
	}
	// Resolve best-effort onto this request's account copy (accounts normally
	// already carry a ProfileArn, so this is a rare fallback path). Use a fresh
	// token via accessToken() rather than the possibly-stale stored one, so an
	// account with an empty profileArn but a valid refreshToken self-heals.
	tok, err := c.accessToken(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn for account %s: %w", c.id, err)
	}
	arn, err := resolveProfileArn(ctx, c.client, c.apiRegion(), tok)
	if err != nil {
		return "", fmt.Errorf("resolve profileArn for account %s: %w", c.id, err)
	}

	// Resolve the remaining identity before the single fenced write: filling an
	// empty profileArn starts a new lifecycle, so a second write with this old
	// revision would correctly be rejected.
	var email, userID string
	if c.acct.Email == "" || c.acct.UserID == "" {
		ident := fetchAccountIdentity(ctx, c.client, c.apiRegion(), arn, tok)
		email, userID = ident.Email, ident.UserID
	}
	c.acct.ProfileArn = arn
	if email != "" {
		c.acct.Email = email
	}
	if userID != "" {
		c.acct.UserID = userID
	}
	// Best-effort persistence. The lifecycle bump invalidates this lease; the
	// pre-send check requeues it with the stored identity before runtime traffic.
	_ = c.store.updateIdentityAtRevision(c.id, c.revision, arn, email, userID)
	return arn, nil
}

func (c *accountCreds) accessToken(ctx context.Context) (string, error) {
	// Refresh proactively when the token is missing or near expiry. The
	// background refresher normally keeps it fresh; this covers the gap.
	exp := c.acct.expiry()
	needs := c.acct.AccessToken == "" ||
		(!exp.IsZero() && time.Now().Add(tokenRefreshBuffer).After(exp))
	if needs {
		if err := c.refresh(ctx); err != nil {
			// If we still hold a token, use it and let the backend decide.
			if c.acct.AccessToken != "" {
				return c.acct.AccessToken, nil
			}
			return "", err
		}
	}
	return c.acct.AccessToken, nil
}

func (c *accountCreds) refresh(ctx context.Context) error {
	// Route through the store so concurrent refreshes of this account (across
	// requests and the background refresher) collapse into one CreateToken call.
	fresh, err := c.store.refreshTokenAtCredential(ctx, c.client, c.id, c.credential)
	if err != nil {
		return err
	}
	c.acct = fresh
	return nil
}

// ---------------------------------------------------------------------------
// Account selector: revisioned, generation-stamped routing state.
// ---------------------------------------------------------------------------

// accountCooldown is how long a failing account is skipped before it is retried.
const accountCooldown = 60 * time.Second

// depletedFallbackTTL is retained as retry/probe metadata when upstream does not
// provide a reset time. Depletion never expires into eligibility by wall clock.
const depletedFallbackTTL = 45 * time.Minute

type quotaEligibility uint8

const (
	quotaUnknown quotaEligibility = iota
	quotaBase
	quotaOverage
	quotaDepleted
)

type usageObservationSource uint8

const (
	usageObservationAuthoritative usageObservationSource = iota
	usageObservationProbe
)

// selectorAccountState is the selector-owned state for one account revision.
// generation changes on every mutation so in-flight results can be applied with
// compare-and-swap semantics rather than wall-clock ordering.
type selectorAccountState struct {
	revision      uint64
	lifecycle     uint64
	generation    uint64
	quota         quotaEligibility
	cooldownUntil time.Time
	retryAfter    time.Time
	reactive      bool
}

// accountLease stamps a routing decision. Success is accepted only while both
// its account revision and selector generation are still current.
type accountLease struct {
	creds      *accountCreds
	revision   uint64
	generation uint64
	fallback   bool
}

// usageStamp identifies the routing state observed before a GetUsage request.
// A result from an older revision or generation is ignored.
type usageStamp struct {
	id             string
	revision       uint64
	generation     uint64
	overageEnabled bool
}

// usageReconcileTarget pins both the selector state and the observation rules
// chosen by the periodic reconciler. A queued target is discarded if its stamp
// changes before or during the fetch.
type usageReconcileTarget struct {
	stamp      usageStamp
	source     usageObservationSource
	priorQuota quotaEligibility
}

type leaseSendStatus uint8

const (
	leaseSendInvalid leaseSendStatus = iota
	leaseSendReady
	leaseSendSuperseded
)

type leaseRoutingClass uint8

const (
	leaseRoutingFallback leaseRoutingClass = iota
	leaseRoutingOverage
	leaseRoutingPreferred
)

// pickResult either carries a directly routable lease or identifies one strict
// unknown account that must receive a fresh usage check before selection retries.
type pickResult struct {
	lease         *accountLease
	verifyID      string
	knownDepleted bool
}

// accountSelector picks accounts round-robin and owns all ephemeral routing
// state. AccountStore.mu must be acquired before accountSelector.mu whenever
// both are needed.
type accountSelector struct {
	store  *AccountStore
	client *http.Client

	mu             sync.Mutex
	index          int
	states         map[string]*selectorAccountState
	nextGeneration uint64
}

func newAccountSelector(store *AccountStore, client *http.Client) *accountSelector {
	return &accountSelector{
		store:  store,
		client: client,
		states: map[string]*selectorAccountState{},
	}
}

func (s *accountSelector) nextGenerationLocked() uint64 {
	s.nextGeneration++
	if s.nextGeneration == 0 {
		s.nextGeneration++
	}
	return s.nextGeneration
}

// stateForRuntimeLocked returns state for rt, resetting an older revision to
// conservative quotaUnknown. A stale store snapshot is never allowed to roll a
// newer selector state backward.
func (s *accountSelector) stateForRuntimeLocked(rt accountRuntime) *selectorAccountState {
	st := s.states[rt.Account.ID]
	if st != nil && st.revision == rt.Revision && st.lifecycle == rt.Lifecycle {
		return st
	}
	if st != nil && st.revision > rt.Revision {
		return nil
	}
	if st != nil && st.lifecycle == rt.Lifecycle {
		// Disabled/overage policy mutations invalidate leases via Revision while
		// retaining sticky depletion and cooldown for the same credentials. Preserve
		// the existing conservative policy behavior: a routable state must prove
		// positive base credit again after overage is disabled.
		st.revision = rt.Revision
		if !rt.Account.OverageEnabled && (st.quota == quotaBase || st.quota == quotaOverage) {
			st.quota = quotaUnknown
		}
		st.generation = s.nextGenerationLocked()
		return st
	}
	st = &selectorAccountState{
		revision:   rt.Revision,
		lifecycle:  rt.Lifecycle,
		generation: s.nextGenerationLocked(),
		quota:      quotaUnknown,
	}
	s.states[rt.Account.ID] = st
	return st
}

func (s *accountSelector) mutateLocked(st *selectorAccountState) {
	st.generation = s.nextGenerationLocked()
}

func (s *accountSelector) pruneStatesLocked(list []accountRuntime) {
	live := make(map[string]bool, len(list))
	for _, rt := range list {
		live[rt.Account.ID] = true
	}
	for id := range s.states {
		if !live[id] {
			delete(s.states, id)
		}
	}
}

// pickFor chooses the next immediately eligible account. With affinityKey it
// uses rendezvous hashing so one client session returns to the same eligible
// account without storing session state. Without affinityKey it retains the
// existing round-robin order. Ready base/unknown accounts are preferred over
// ready accounts known to be using overage. Strict no-overage accounts are
// selectable only in quotaBase. If no account can be routed immediately,
// verifyID names a strict quotaUnknown account that should receive a fresh
// GetUsage before selection retries. Only cooldown participates in the
// all-skipped fallback; depleted accounts are never fallback candidates.
func (s *accountSelector) pickFor(tried map[string]bool, affinityKey string) pickResult {
	list := s.store.RuntimeList()
	if len(list) == 0 {
		return pickResult{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStatesLocked(list)

	affinity := affinityKey != ""
	now := time.Now()
	var preferred *accountLease
	var preferredScore uint64
	var overage *accountLease
	var overageScore uint64
	var overagePos int
	var overageKnownDepleted bool
	var verifyID string
	var verifyScore uint64
	knownDepleted := false
	var fallback *accountLease
	var fallbackAt time.Time
	var fallbackScore uint64
	n := len(list)
	for i := 0; i < n; i++ {
		pos := i
		if !affinity {
			pos = (s.index + i) % n
		}
		rt := list[pos]
		a := rt.Account
		if tried[a.ID] || !accountUsable(a) {
			continue
		}
		st := s.stateForRuntimeLocked(rt)
		if st == nil {
			continue
		}

		switch st.quota {
		case quotaDepleted:
			knownDepleted = true
			continue
		case quotaUnknown:
			if !a.OverageEnabled {
				if !affinity {
					if verifyID == "" {
						verifyID = a.ID
					}
				} else {
					score := accountAffinityScore(affinityKey, a.ID)
					if verifyID == "" || score > verifyScore || (score == verifyScore && a.ID < verifyID) {
						verifyID, verifyScore = a.ID, score
					}
				}
				continue
			}
		}

		creds := s.credsFor(rt)
		lease := &accountLease{
			creds:      creds,
			revision:   rt.Revision,
			generation: st.generation,
		}
		if now.Before(st.cooldownUntil) {
			lease.fallback = true
			score := uint64(0)
			if affinity {
				score = accountAffinityScore(affinityKey, a.ID)
			}
			if fallback == nil || st.cooldownUntil.Before(fallbackAt) ||
				(affinity && st.cooldownUntil.Equal(fallbackAt) &&
					(score > fallbackScore || (score == fallbackScore && a.ID < fallback.creds.id))) {
				fallback, fallbackAt, fallbackScore = lease, st.cooldownUntil, score
			}
			continue
		}

		isOverage := st.quota == quotaOverage
		if !affinity {
			if isOverage {
				if overage == nil {
					overage = lease
					overagePos = pos
					overageKnownDepleted = knownDepleted
				}
				continue
			}
			s.index = (pos + 1) % n
			return pickResult{lease: lease, knownDepleted: knownDepleted}
		}

		score := accountAffinityScore(affinityKey, a.ID)
		if isOverage {
			if overage == nil || score > overageScore || (score == overageScore && a.ID < overage.creds.id) {
				overage, overageScore = lease, score
			}
			continue
		}
		if preferred == nil || score > preferredScore || (score == preferredScore && a.ID < preferred.creds.id) {
			preferred, preferredScore = lease, score
		}
	}
	if preferred != nil {
		return pickResult{lease: preferred, knownDepleted: knownDepleted}
	}
	if overage != nil {
		if !affinity {
			s.index = (overagePos + 1) % n
			knownDepleted = overageKnownDepleted
		}
		return pickResult{lease: overage, knownDepleted: knownDepleted}
	}
	if verifyID != "" {
		return pickResult{verifyID: verifyID, knownDepleted: knownDepleted}
	}
	return pickResult{lease: fallback, knownDepleted: knownDepleted}
}

func accountAffinityScore(affinityKey, accountID string) uint64 {
	sum := sha256.Sum256([]byte(affinityKey + "\x00" + accountID))
	return binary.BigEndian.Uint64(sum[:8])
}

// credsFor builds accountCreds for an atomic runtime snapshot.
func (s *accountSelector) credsFor(rt accountRuntime) *accountCreds {
	return &accountCreds{
		store: s.store, client: s.client, id: rt.Account.ID,
		revision: rt.Revision, credential: rt.Credential, acct: rt.Account,
	}
}

// accountUsable reports whether an account can serve a request at all.
func accountUsable(a StoredAccount) bool {
	if a.Disabled {
		return false
	}
	if a.ProfileArn != "" {
		return true
	}
	return a.RefreshToken != "" && a.ClientID != "" && a.ClientSecret != ""
}

// peekAny returns credentials for one usable account for control-plane calls.
// It intentionally ignores quota state and does not advance round-robin order.
func (s *accountSelector) peekAny() (*accountCreds, bool) {
	list := s.store.RuntimeList()
	if len(list) == 0 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneStatesLocked(list)
	now := time.Now()
	var first *accountCreds
	for _, rt := range list {
		if !accountUsable(rt.Account) {
			continue
		}
		st := s.stateForRuntimeLocked(rt)
		if st == nil {
			continue
		}
		creds := s.credsFor(rt)
		if first == nil {
			first = creds
		}
		if !now.Before(st.cooldownUntil) {
			return creds, true
		}
	}
	return first, first != nil
}

// byID returns revision-stamped credentials for one account regardless of its
// disabled, usability, quota, or cooldown state.
func (s *accountSelector) byID(id string) (*accountCreds, bool) {
	rt, ok := s.store.Runtime(id)
	if !ok {
		return nil, false
	}
	return s.credsFor(rt), true
}

// listAll returns revision-stamped credentials for every stored account.
func (s *accountSelector) listAll() []*accountCreds {
	list := s.store.RuntimeList()
	out := make([]*accountCreds, 0, len(list))
	for _, rt := range list {
		out = append(out, s.credsFor(rt))
	}
	return out
}

// validateForSend checks a lease immediately before a runtime send. It
// linearizes account mutation at AccountStore.mu without holding either lock
// across I/O. A ready account in a strictly higher routing class may supersede
// an otherwise valid lease; the scan never advances the round-robin cursor.
func (s *accountSelector) validateForSend(lease *accountLease, tried map[string]bool) leaseSendStatus {
	if lease == nil || lease.creds == nil {
		return leaseSendInvalid
	}
	status := leaseSendInvalid
	s.store.withRuntimeList(func(list []accountRuntime) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.pruneStatesLocked(list)

		var selected *accountRuntime
		for i := range list {
			if list[i].Account.ID == lease.creds.id {
				selected = &list[i]
				break
			}
		}
		if selected == nil || selected.Revision != lease.revision || !accountUsable(selected.Account) {
			return
		}
		st := s.states[lease.creds.id]
		if st == nil || st.revision != lease.revision || st.generation != lease.generation {
			return
		}
		if st.quota == quotaDepleted || (!selected.Account.OverageEnabled && st.quota != quotaBase) {
			return
		}
		now := time.Now()
		cooling := now.Before(st.cooldownUntil)
		if cooling != lease.fallback {
			return
		}

		selectedClass := leaseRoutingPreferred
		switch {
		case lease.fallback:
			selectedClass = leaseRoutingFallback
		case st.quota == quotaOverage:
			selectedClass = leaseRoutingOverage
		}
		status = leaseSendReady

		for _, rt := range list {
			a := rt.Account
			if a.ID == lease.creds.id || tried[a.ID] || !accountUsable(a) {
				continue
			}
			candidate := s.stateForRuntimeLocked(rt)
			if candidate == nil || candidate.quota == quotaDepleted || now.Before(candidate.cooldownUntil) {
				continue
			}
			if candidate.quota == quotaUnknown && !a.OverageEnabled {
				continue
			}
			candidateClass := leaseRoutingPreferred
			if candidate.quota == quotaOverage {
				candidateClass = leaseRoutingOverage
			}
			if candidateClass > selectedClass {
				status = leaseSendSuperseded
				return
			}
		}
	})
	return status
}

// recordSuccess clears cooldown only when the exact selected generation remains
// current. Runtime success never promotes unknown or depleted quota state.
func (s *accountSelector) recordSuccess(lease *accountLease) {
	s.withLeaseState(lease, true, func(st *selectorAccountState) {
		if st.cooldownUntil.IsZero() {
			return
		}
		st.cooldownUntil = time.Time{}
		s.mutateLocked(st)
	})
}

// recordFailure conservatively cools the current account revision. A failure may
// supersede an older success generation, but never a replaced/re-added account.
func (s *accountSelector) recordFailure(lease *accountLease) {
	s.withLeaseState(lease, false, func(st *selectorAccountState) {
		st.cooldownUntil = time.Now().Add(accountCooldown)
		s.mutateLocked(st)
	})
}

// recordDepleted records a reactive quota failure. It is sticky until a fresh,
// matching usage observation reports positive base Remaining and is never an
// all-skipped fallback candidate.
func (s *accountSelector) recordDepleted(lease *accountLease) {
	s.withLeaseState(lease, false, func(st *selectorAccountState) {
		retryAfter := time.Now().Add(depletedFallbackTTL)
		if st.retryAfter.After(retryAfter) {
			retryAfter = st.retryAfter
		}
		st.quota = quotaDepleted
		st.reactive = true
		st.retryAfter = retryAfter
		s.mutateLocked(st)
	})
}

func (s *accountSelector) withLeaseState(lease *accountLease, requireGeneration bool, fn func(*selectorAccountState)) bool {
	if lease == nil || lease.creds == nil {
		return false
	}
	return s.store.withRuntime(lease.creds.id, func(rt accountRuntime) bool {
		if rt.Revision != lease.revision {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.states[lease.creds.id]
		if st == nil || st.revision != lease.revision {
			return false
		}
		if requireGeneration && st.generation != lease.generation {
			return false
		}
		fn(st)
		return true
	})
}

// usageTarget captures credentials and a selector stamp before GetUsage starts.
func (s *accountSelector) usageTarget(id string) (*accountCreds, usageStamp, bool) {
	var creds *accountCreds
	var stamp usageStamp
	ok := s.store.withRuntime(id, func(rt accountRuntime) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.stateForRuntimeLocked(rt)
		if st == nil {
			return false
		}
		creds = s.credsFor(rt)
		stamp = usageStamp{
			id: id, revision: rt.Revision, generation: st.generation,
			overageEnabled: rt.Account.OverageEnabled,
		}
		return true
	})
	return creds, stamp, ok
}

// depletedDeadline derives retry/probe metadata from usage. Elapsing this time
// never changes quota eligibility.
func depletedDeadline(u *kiroUsage, now time.Time) time.Time {
	if u != nil && u.ResetAt != "" {
		if t, err := time.Parse(time.RFC3339, u.ResetAt); err == nil && t.After(now) {
			return t
		}
	}
	return now.Add(depletedFallbackTTL)
}

// applyUsage applies a GetUsage result only if the account revision and selector
// generation still match the pre-fetch stamp. Positive base Remaining assigns
// the preferred base tier and is the sole way to admit or recover a strict
// account or clear a reactive depletion. Overage-enabled unknown accounts remain
// availability-first until an authoritative observation classifies them.
func (s *accountSelector) applyUsage(stamp usageStamp, u *kiroUsage, source usageObservationSource) bool {
	if stamp.id == "" || u == nil || u.Credit == nil {
		return false
	}
	return s.store.withRuntime(stamp.id, func(rt accountRuntime) bool {
		if rt.Revision != stamp.revision || rt.Account.OverageEnabled != stamp.overageEnabled {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		st := s.states[stamp.id]
		if st == nil || st.revision != stamp.revision || st.generation != stamp.generation {
			return false
		}

		now := time.Now()
		if u.Credit.Remaining > 0 {
			changed := st.quota != quotaBase || st.reactive || !st.retryAfter.IsZero()
			st.quota = quotaBase
			st.reactive = false
			st.retryAfter = time.Time{}
			if changed {
				s.mutateLocked(st)
			}
			return true
		}

		// A base-zero observation can authorize unused overage only before a
		// reactive quota failure. Probe observations cannot make this optimistic
		// transition because upstream may clamp usage at the base limit.
		if !st.reactive && source != usageObservationProbe && stamp.overageEnabled &&
			overageActive(u) && overageRemaining(u.Credit) > 0 {
			changed := st.quota != quotaOverage || !st.retryAfter.IsZero()
			st.quota = quotaOverage
			st.retryAfter = time.Time{}
			if changed {
				s.mutateLocked(st)
			}
			return true
		}

		st.quota = quotaDepleted
		deadline := depletedDeadline(u, now)
		if st.reactive && st.retryAfter.After(deadline) {
			deadline = st.retryAfter
		}
		st.retryAfter = deadline
		s.mutateLocked(st)
		return true
	})
}

// reconcileTargets returns the accounts whose quota class must be refreshed.
// Depleted and strict unknown accounts use conservative Probe semantics;
// routable overage-enabled accounts use authoritative observations so periodic
// reconciliation can detect both base exhaustion and base-credit recovery.
func (s *accountSelector) reconcileTargets() []usageReconcileTarget {
	var targets []usageReconcileTarget
	s.store.withRuntimeList(func(list []accountRuntime) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.pruneStatesLocked(list)
		targets = make([]usageReconcileTarget, 0, len(list))
		for _, rt := range list {
			a := rt.Account
			if !accountUsable(a) {
				continue
			}
			st := s.stateForRuntimeLocked(rt)
			if st == nil {
				continue
			}

			var source usageObservationSource
			switch {
			case st.quota == quotaDepleted:
				source = usageObservationProbe
			case !a.OverageEnabled && st.quota == quotaUnknown:
				source = usageObservationProbe
			case a.OverageEnabled && st.quota != quotaDepleted:
				source = usageObservationAuthoritative
			default:
				continue
			}
			targets = append(targets, usageReconcileTarget{
				stamp: usageStamp{
					id: a.ID, revision: rt.Revision, generation: st.generation,
					overageEnabled: a.OverageEnabled,
				},
				source:     source,
				priorQuota: st.quota,
			})
		}
	})
	return targets
}

// forget discards selector state after account removal. Old observations remain
// harmless because their runtime revision no longer exists.
func (s *accountSelector) forget(id string) {
	s.mu.Lock()
	delete(s.states, id)
	s.mu.Unlock()
}

func (s *accountSelector) isReactivelyDepleted(id string, revision uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[id]
	return st != nil && st.revision == revision && st.quota == quotaDepleted && st.reactive
}

func (s *accountSelector) isDepletedAtRevision(id string, revision uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[id]
	return st != nil && st.revision == revision && st.quota == quotaDepleted
}

// isDepleted is a temporary compatibility query for callers not yet migrated to
// revision-aware aggregate inspection.
func (s *accountSelector) isDepleted(id string) bool {
	rt, ok := s.store.Runtime(id)
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[id]
	return st != nil && st.revision == rt.Revision && st.quota == quotaDepleted
}

// overageRemaining reports how much configured overage budget remains.
func overageRemaining(c *kiroCreditUsage) float64 {
	if c == nil || c.OverageCap <= 0 {
		return 0
	}
	spent := c.Used - c.Limit
	if spent < 0 {
		spent = 0
	}
	if remaining := c.OverageCap - spent; remaining > 0 {
		return remaining
	}
	return 0
}

// overageActive reports whether upstream has enabled overage for the account.
func overageActive(u *kiroUsage) bool {
	if u == nil {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(u.OverageStatus)) {
	case "ENABLED", "ACTIVE":
		return true
	}
	return false
}

// isAccountFailure reports whether err is an upstream failure that warrants
// trying a different account (auth, quota/throttle, upstream 5xx, or a
// transport error). A 400 that is not a thinking-signature issue is a problem
// with the request itself and should surface immediately without burning other
// accounts.
func isAccountFailure(err error) bool {
	if err == nil {
		return false
	}
	he, ok := err.(*kiroHTTPError)
	if !ok {
		// Transport/stream error (e.g. connection reset): try another account.
		return true
	}
	switch {
	case he.Status == http.StatusUnauthorized, he.Status == http.StatusForbidden:
		return true
	case he.Status == http.StatusPaymentRequired, he.Status == http.StatusTooManyRequests:
		return true
	case he.Status == 423: // Locked (account suspended)
		return true
	case he.Status >= 500:
		return true
	default:
		return false
	}
}

// isAccountDepleted reports whether err signals the account is out of credit:
// HTTP 402 (payment required) or 423 (locked/suspended), or an in-stream
// serviceQuotaExceededError event (surfaced via the error's Kind). Such errors
// park the account in the depleted map until reset_at/fallback rather than the
// short cooldown, so it is not retried every 60s. It is a strict subset of
// isAccountFailure; a 429 throttle stays on the short cooldown.
func isAccountDepleted(err error) bool {
	if err == nil {
		return false
	}
	he, ok := err.(*kiroHTTPError)
	if !ok {
		return false
	}
	if he.Status == http.StatusPaymentRequired || he.Status == http.StatusLocked {
		return true
	}
	kind := normalizeKiroEventKind(he.Kind)
	return kind == "servicequotaexceedederror" || kind == "servicequotaexceededexception"
}

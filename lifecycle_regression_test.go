package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountLifecycleChangesOnlyForCredentialIdentityBoundaries(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{
		ID: "acc", ClientID: "client", ClientSecret: "secret", RefreshToken: "refresh",
		AccessToken: "access", ProfileArn: "arn:old", CreatedAt: "1",
	}))
	initial, ok := store.Runtime("acc")
	require.True(t, ok)
	require.NotZero(t, initial.Lifecycle)
	require.NotZero(t, initial.Credential)

	require.NoError(t, store.UpdateLabel("acc", "label"))
	require.NoError(t, store.UpdateTokens("acc", "new-access", "new-refresh", "2030-01-01T00:00:00Z"))
	require.NoError(t, store.SetDisabled("acc", true))
	require.NoError(t, store.SetOverageEnabled("acc", true))
	policy, ok := store.Runtime("acc")
	require.True(t, ok)
	assert.Greater(t, policy.Revision, initial.Revision)
	assert.Equal(t, initial.Lifecycle, policy.Lifecycle)
	assert.Equal(t, initial.Credential, policy.Credential)

	require.NoError(t, store.UpdateIdentity("acc", "arn:new", "", ""))
	profile, ok := store.Runtime("acc")
	require.True(t, ok)
	assert.Greater(t, profile.Lifecycle, policy.Lifecycle)
	assert.Equal(t, policy.Credential, profile.Credential)

	fresh := profile.Account
	fresh.ClientID = "replacement-client"
	require.NoError(t, store.ReplaceCredentials("acc", &fresh))
	replacement, ok := store.Runtime("acc")
	require.True(t, ok)
	assert.Greater(t, replacement.Lifecycle, profile.Lifecycle)
	assert.Greater(t, replacement.Credential, profile.Credential)

	encoded, err := json.Marshal(replacement.Account)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "lifecycle")
	assert.NotContains(t, string(encoded), "credential")
	assert.NotContains(t, replacement.Account.view(), "lifecycle")
	assert.NotContains(t, replacement.Account.view(), "credential")
}

func TestAccountLifecycleChangesWhenProfileBecomesKnown(t *testing.T) {
	store, err := NewAccountStore(filepath.Join(t.TempDir(), "accounts.json"))
	require.NoError(t, err)
	require.NoError(t, store.Add(&StoredAccount{ID: "acc", CreatedAt: "1"}))
	before, ok := store.Runtime("acc")
	require.True(t, ok)

	require.NoError(t, store.UpdateIdentity("acc", "arn:resolved", "user@example.com", "user"))
	after, ok := store.Runtime("acc")
	require.True(t, ok)
	assert.Greater(t, after.Revision, before.Revision)
	assert.Greater(t, after.Lifecycle, before.Lifecycle)
}

func TestSelectorPolicyRevisionPreservesReactiveDepletion(t *testing.T) {
	s := newTestSelector(t, "acc")
	lease := requireLease(t, s.pick(map[string]bool{}))
	s.recordDepleted(lease)

	require.NoError(t, s.store.SetOverageEnabled("acc", false))
	assert.Nil(t, s.pick(map[string]bool{}).lease)
	strictRevision := runtimeRevision(t, s.store, "acc")
	assert.True(t, s.isReactivelyDepleted("acc", strictRevision))
	assert.Empty(t, s.pick(map[string]bool{}).verifyID, "depleted is not downgraded to unknown")

	require.NoError(t, s.store.SetOverageEnabled("acc", true))
	assert.Nil(t, s.pick(map[string]bool{}).lease)
	overageRevision := runtimeRevision(t, s.store, "acc")
	assert.True(t, s.isReactivelyDepleted("acc", overageRevision))
	assert.Contains(t, reconciliationTargetIDs(s), "acc")
}

func TestSelectorCredentialLifecycleResetsReactiveDepletion(t *testing.T) {
	s := newTestSelector(t, "acc")
	old := requireLease(t, s.pick(map[string]bool{}))
	s.recordDepleted(old)
	stored, ok := s.store.Get("acc")
	require.True(t, ok)
	stored.ClientID = "replacement-client"
	require.NoError(t, s.store.ReplaceCredentials("acc", &stored))

	fresh := requireLease(t, s.pick(map[string]bool{}))
	assert.NotEqual(t, old.revision, fresh.revision)
	assert.False(t, s.isReactivelyDepleted("acc", fresh.revision))
	assert.Equal(t, quotaUnknown, selectorQuota(t, s, "acc"))
}

package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayTopology exercises relay topology on the supplied native store.
// R3/R4: only explicitly negotiated relay bindings may serve descendants.
// Root pins and the bounded authority path are configured independently from
// source actor labels; legacy direct-spoke behavior remains unchanged.
func RunRelayTopology(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", PushEnabled: true, Enabled: true}
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "personal-member", AdminActor: "admin", PlaintextToken: "topology-parent-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	enroll := db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "topology-leaf-test-token"}
	_, err = store.CreateRelayEnrollment(ctx, enroll)
	require.Error(t, err, "legacy spoke cannot silently become a relay")
	configuration := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000005", UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "personal-member", ServeDownstream: true, ResetEpoch: 1}
	configured, err := store.SetRelayBindingConfig(ctx, project.ID, configuration)
	require.NoError(t, err)
	require.Equal(t, &configuration, configured.RelayConfig)
	read, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, configured, read)
	child, err := store.CreateRelayEnrollment(ctx, enroll)
	require.NoError(t, err)
	require.Equal(t, "personal-member", child.Enrollment.Actor)
	_, err = store.AuthorizeFederationToken(ctx, child.Token, project.ID, "pull")
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.Error(t, err, "detach must refuse active descendants before any partial teardown")
	_, err = store.AuthorizeFederationToken(ctx, child.Token, project.ID, "pull")
	require.NoError(t, err, "refused detach preserves the descendant grant")
	issue, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Preserve source identity", Author: "source-assistant"})
	require.NoError(t, err)
	require.Equal(t, "source-assistant", issue.Author)
	require.Equal(t, "source-assistant", event.Actor)
	offered, err := store.PendingRelayDeliveries(ctx, configuration.BindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Len(t, offered, 1)
	require.Equal(t, rootUID, offered[0].ReceiverInstanceUID)
	require.Equal(t, event.UID, offered[0].SourceUID)
	require.Equal(t, event.ContentHash, offered[0].SourceHash)
	decoded, err := db.DecodeRelaySourceEvent(offered[0].Body)
	require.NoError(t, err)
	require.Equal(t, db.RemoteEventFromStored(event), decoded)
	// R4/A8: destructive replica purge cannot discard projection state while
	// its immutable source intent is still awaiting the root.
	_, err = store.PurgeIssue(ctx, issue.ID, "personal-member", nil)
	require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported)
	retained, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, issue.Title, retained.Title)
	retried, err := store.PendingRelayDeliveries(ctx, configuration.BindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Equal(t, offered, retried, "refused purge preserves exact pending delivery")
	// Callers commonly read a binding before pausing it; unchanged negotiated
	// state must survive that ordinary control update, without allowing injection.
	paused := read
	paused.Enabled = false
	pausedResult, err := store.UpsertFederationBinding(ctx, paused)
	require.NoError(t, err)
	require.Equal(t, &configuration, pausedResult.RelayConfig)
	_, err = store.PendingRelayDeliveries(ctx, configuration.BindingUID, db.RelayStreamEvent, 10)
	require.Error(t, err)
	paused.Enabled = true
	_, err = store.UpsertFederationBinding(ctx, paused)
	require.NoError(t, err)
	injected := read
	changed := configuration
	changed.ServeDownstream = false
	injected.RelayConfig = &changed
	_, err = store.UpsertFederationBinding(ctx, injected)
	require.Error(t, err, "generic control writes cannot inject configuration")
	// Legacy control writes must retain negotiated topology rather than erase it.
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	read, err = store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, &configuration, read.RelayConfig)
	for _, change := range []func(*db.RelayBindingConfig){
		func(c *db.RelayBindingConfig) { c.ProtocolVersion++ },
		func(c *db.RelayBindingConfig) { c.UpstreamInstanceUID = store.InstanceUID() },
		func(c *db.RelayBindingConfig) { c.AuthorityUID = store.InstanceUID() },
		func(c *db.RelayBindingConfig) { c.HubPath = []string{rootUID, rootUID, store.InstanceUID()} },
		func(c *db.RelayBindingConfig) { c.HubPath = []string{rootUID} },
		func(c *db.RelayBindingConfig) {
			c.HubPath = append(c.HubPath, []string{"00000000000000000000000011", "00000000000000000000000012", "00000000000000000000000013", "00000000000000000000000014", "00000000000000000000000015", "00000000000000000000000016", "00000000000000000000000017"}...)
		},
		func(c *db.RelayBindingConfig) { c.BindingUID = "00000000000000000000000007" },
		func(c *db.RelayBindingConfig) { c.ResetEpoch = 0 },
	} {
		invalid := configuration
		invalid.HubPath = append([]string(nil), configuration.HubPath...)
		change(&invalid)
		_, err = store.SetRelayBindingConfig(ctx, project.ID, invalid)
		require.Error(t, err)
	}
	other := binding
	other.HubURL = "https://other-hub.example"
	_, err = store.UpsertFederationBinding(ctx, other)
	require.Error(t, err, "a generic upsert must not change the retained upstream authority")
	captured := store.FederationEnrollmentTransactionFence(child.Enrollment, project.ID, "push")
	revoked := configuration
	revoked.UpstreamRevoked = true
	_, err = store.SetRelayBindingConfig(ctx, project.ID, revoked)
	require.NoError(t, err)
	_, err = store.PendingRelayDeliveries(ctx, configuration.BindingUID, db.RelayStreamEvent, 10)
	require.Error(t, err)
	_, err = store.AuthorizeFederationToken(ctx, child.Token, project.ID, "pull")
	require.Error(t, err)
	_, _, err = store.CreateIssue(db.WithAdditionalTransactionFence(ctx, captured), db.CreateIssueParams{ProjectID: project.ID, Title: "Upstream revoked", Author: "personal-member"})
	require.Error(t, err)
}

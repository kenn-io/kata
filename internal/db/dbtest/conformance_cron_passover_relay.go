package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Contract: each one-event relay page exposes the cutoff before enable.
func checkCronDiscardRelay(t *testing.T, root db.Storage, b Backend) error {
	ctx := t.Context()
	project, err := root.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, root, project)
	require.False(t, job.Definition.Enabled)
	_, err = root.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, root.PinRootAuthority(ctx, pin))
	peer := b.Open(t)
	t.Cleanup(func() { require.NoError(t, peer.Close()) })
	replica, err := peer.CreateProjectWithUID(ctx, "spoke-project", project.UID)
	require.NoError(t, err)
	_, err = peer.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "operator", PushEnabled: true, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, peer.PinRootAuthority(ctx, pin))
	parent, _, err := root.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "operator", AdminActor: "admin", PlaintextToken: "discard-relay-parent-fixture"})
	require.NoError(t, err)
	// #nosec G101 -- Isolated enrollment fixture, not a production credential.
	grant, err := root.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion, Token: "discard-relay-child-fixture"})
	require.NoError(t, err)
	binding := grant.Enrollment.RelayBindingUID
	_, err = peer.SetRelayBindingConfig(ctx, replica.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: binding, UpstreamInstanceUID: root.InstanceUID(), AuthorityUID: root.InstanceUID(), HubPath: []string{root.InstanceUID(), peer.InstanceUID()}, LocalActor: "operator", ResetEpoch: 1})
	require.NoError(t, err)
	deliverOne := func() bool {
		page, err := root.PendingRelayDeliveries(ctx, binding, db.RelayStreamEvent, 1)
		require.NoError(t, err)
		if len(page) == 0 {
			return false
		}
		require.Len(t, page, 1)
		_, err = peer.AcceptRelayDeliveries(ctx, binding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: page})
		require.NoError(t, err)
		require.NoError(t, root.AckRelayDeliveries(ctx, binding, 1, db.RelayStreamEvent, page[0].Sequence, page[0].Digest))
		return true
	}
	for i := 0; deliverOne(); i++ {
		require.Less(t, i, 20, "bootstrap must terminate")
	}
	copied, err := peer.CronJob(ctx, replica.ID, job.UID)
	require.NoError(t, err)
	require.False(t, copied.Definition.Enabled)
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: root.InstanceUID(), PrivateKey: private}, "operator")
	resumed, err := root.ResumeCronJob(writeCtx, db.ResumeCronJobIn{ProjectID: project.ID, UID: job.UID, Actor: "operator", DiscardMissed: true, ExpectedEventUID: job.DefinitionEventUID})
	require.NoError(t, err)
	require.Len(t, resumed.Events, 2)
	require.True(t, deliverOne())
	copied, err = peer.CronJob(ctx, replica.ID, job.UID)
	require.NoError(t, err)
	require.False(t, copied.Definition.Enabled)
	policy, err := peer.ProjectByID(ctx, replica.ID)
	require.NoError(t, err)
	require.NotNil(t, db.CronPassoverFromMetadata(policy.Metadata, job.UID))
	require.True(t, deliverOne())
	copied, err = peer.CronJob(ctx, replica.ID, job.UID)
	require.NoError(t, err)
	require.True(t, copied.Definition.Enabled)
	require.False(t, deliverOne())
	_, err = peer.ResumeCronJob(ctx, db.ResumeCronJobIn{ProjectID: replica.ID, UID: job.UID, Actor: "operator", DiscardMissed: true, ExpectedEventUID: copied.DefinitionEventUID})
	require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported)
	return nil
}

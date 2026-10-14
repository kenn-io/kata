package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayResetRepeatedBeforeDownstreamActivation installs two root resets
// before the downstream activates the first forwarded epoch. Rows the first
// forward queued for that epoch must not outlive the second checkpoint: an
// issue accepted after the first capture and purged before the second would
// otherwise reach the downstream once it activates.
func RunRelayResetRepeatedBeforeDownstreamActivation(t *testing.T, store db.Storage) {
	ctx := t.Context()
	installer, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native relay reset installation is required")
	project, err := store.CreateProject(ctx, "relay-project")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	rootPublic, rootPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(rootPublic), PublicKey: rootPublic}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "relay-member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	upstream, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: upstream, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "relay-member", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
	root := &rootResetFixture{t: t, project: project, pin: pin, private: rootPrivate, rootUID: rootUID, relayUID: store.InstanceUID(), binding: upstream}

	kept := root.created("Kept issue", 300, 1)
	manifest, snapshot, translation := root.checkpoint(2, 2, kept)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "relay-member", AdminActor: "admin", PlaintextToken: "repeat-reset-parent-test-token"})
	require.NoError(t, err)
	deviceUID := "00000000000000000000000008"
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	device, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: deviceUID, ProtocolVersion: db.RelayProtocolVersion, Token: "repeat-reset-device-test-token"})
	require.NoError(t, err)
	deviceBinding := device.Enrollment.RelayBindingUID
	drainRelayGrant(t, store, deviceBinding, device.Enrollment.RelayResetEpoch)

	// The root captures a checkpoint, then accepts one more issue that reaches
	// the relay before the relay installs that checkpoint.
	manifest, snapshot, translation = root.checkpoint(3, 3, kept)
	late := root.created("Accepted after capture", 301, 2)
	root.deliverIssue(ctx, store, late, 2, 1)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	_, err = store.IssueByUID(ctx, late.entity.EntityUID, db.IncludeDeletedYes)
	require.NoError(t, err, "root-accepted work newer than the checkpoint survives installation")

	// The root purges that issue and the relay installs the next checkpoint
	// before the downstream ever asked for the first forwarded epoch.
	manifest, snapshot, translation = root.checkpoint(4, 4, kept)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	_, err = store.IssueByUID(ctx, late.entity.EntityUID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)

	forwarded, err := installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, manifest, forwarded.Manifest, "the downstream receives the latest root checkpoint")
	require.Equal(t, device.Enrollment.RelayResetEpoch+1, forwarded.Translation.Authority.Epoch)
	activated := db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch)
	require.Empty(t, pendingSourceUIDs(activated, t, store, deviceBinding, db.RelayStreamEvent), "the first forward's rows for the purged issue must not survive the second checkpoint")
}

package dbtest

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayResetPreparesForSuspendedDownstream checks that a downstream whose
// parent credential expired or was revoked cannot stop its relay from
// installing a root reset. The relay prepares that child's forwarded
// checkpoint as internal bookkeeping; the child's authority is checked only
// when it downloads or activates the checkpoint. setTokenExpiry rewrites a
// token's stored expiration, standing in for the passage of time.
func RunRelayResetPreparesForSuspendedDownstream(t *testing.T, store db.Storage, setTokenExpiry func(context.Context, int64, time.Time) error) {
	for _, suspension := range []string{"expired", "revoked", "live"} {
		t.Run(suspension, func(t *testing.T) {
			runRelayResetSuspendedDownstream(t, store, suspension, setTokenExpiry)
		})
	}
}

func runRelayResetSuspendedDownstream(t *testing.T, store db.Storage, suspension string, setTokenExpiry func(context.Context, int64, time.Time) error) {
	ctx := t.Context()
	installer, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native relay reset installation is required")
	project, err := store.CreateProject(ctx, "relay-project-"+suspension)
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
	purged := root.created("Purged at the root", 301, 2)
	manifest, snapshot, translation := root.checkpoint(2, 2, kept, purged)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	expires := time.Now().UTC().Add(time.Hour)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "relay-member", AdminActor: "admin", PlaintextToken: "suspended-parent-" + suspension + "-test-token", ExpiresAt: &expires})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	device, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "suspended-device-" + suspension + "-test-token"})
	require.NoError(t, err)
	deviceBinding := device.Enrollment.RelayBindingUID
	drainRelayGrant(t, store, deviceBinding, device.Enrollment.RelayResetEpoch)

	switch suspension {
	case "expired":
		require.NoError(t, setTokenExpiry(ctx, parent.ID, time.Now().UTC().Add(-time.Minute)))
	case "revoked":
		_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
		require.NoError(t, err)
	}

	manifest, snapshot, translation = root.checkpoint(3, 3, kept)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation), "a suspended child must not block its relay's own recovery")
	_, err = store.IssueByUID(ctx, purged.entity.EntityUID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound, "the relay installed the root's current state")
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, int64(3), binding.RelayConfig.ResetEpoch)
	record, found := supersededRecord(t, store, project.UID, deviceBinding)
	require.True(t, found, "the child's forwarded checkpoint is prepared regardless of its credential")
	require.Equal(t, manifest.SnapshotUID, record.SnapshotUID)
	require.Equal(t, device.Enrollment.RelayResetEpoch+1, record.NextEpoch)

	if suspension == "live" {
		forwarded, err := installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
		require.NoError(t, err)
		require.Equal(t, manifest, forwarded.Manifest)
		_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch), deviceBinding, db.RelayStreamEvent, 10)
		require.NoError(t, err, "a live child activates the prepared epoch")
		_, found = supersededRecord(t, store, project.UID, deviceBinding)
		require.False(t, found, "activation consumed the prepared transition")
		return
	}
	_, err = installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.Error(t, err, "a suspended child is refused when it downloads the checkpoint")
	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, record.NextEpoch), deviceBinding, db.RelayStreamEvent, 10)
	require.Error(t, err, "a suspended child is refused when it tries to activate the checkpoint")
	_, found = supersededRecord(t, store, project.UID, deviceBinding)
	require.True(t, found, "a refused child keeps its prepared transition")
	if suspension != "expired" {
		return
	}
	// Restoring the credential lets the child recover from the prepared state.
	require.NoError(t, setTokenExpiry(ctx, parent.ID, time.Now().UTC().Add(time.Hour)))
	forwarded, err := installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, manifest, forwarded.Manifest)
	require.Equal(t, record.NextEpoch, forwarded.Translation.Authority.Epoch)
	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch), deviceBinding, db.RelayStreamEvent, 10)
	require.NoError(t, err)
}

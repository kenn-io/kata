package dbtest

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayResetRefreshSupersedesArtifactOffers forces the cached-checkpoint
// refresh path before the downstream activates: the root manifest's history
// boundary is below the relay's own event ids, so the downstream's reset
// download rebuilds the forward. An artifact offer the installation already
// superseded, and that was never emitted, must not block that rebuild; the
// old epoch hides the offer, so a refusal would strand the downstream.
func RunRelayResetRefreshSupersedesArtifactOffers(t *testing.T, store db.Storage) {
	ctx := t.Context()
	installer, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native relay reset installation is required")
	artifacts, ok := store.(db.EmbeddingArtifactStorage)
	require.True(t, ok, "native artifact retention is required")
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
	// A root whose history boundary sits below this relay's event ids: the
	// two databases number events independently.
	root := &rootResetFixture{t: t, project: project, pin: pin, private: rootPrivate, rootUID: rootUID, relayUID: store.InstanceUID(), binding: upstream, historyEventID: 1}

	kept := root.created("Kept issue", 300, 1)
	purged := root.created("Purged at the root", 301, 2)
	manifest, snapshot, translation := root.checkpoint(2, 2, kept, purged)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "relay-member", AdminActor: "admin", PlaintextToken: "refresh-forward-parent-test-token"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	device, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "refresh-forward-device-test-token"})
	require.NoError(t, err)
	deviceBinding := device.Enrollment.RelayBindingUID
	drainRelayGrant(t, store, deviceBinding, device.Enrollment.RelayResetEpoch)

	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: kept.entity.EntityUID, ProducerInstanceUID: rootUID, Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText("Kept issue", ""), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)
	// The offer is owed but never emitted: the device has not polled since.
	owed := unacknowledgedRelayOutbox(t, store, deviceBinding, device.Enrollment.RelayResetEpoch)
	require.Len(t, owed[db.RelayStreamArtifact], 1)
	require.False(t, owed[db.RelayStreamArtifact][0].Emitted)
	drainRelayGrant(t, store, upstream, 2)

	manifest, snapshot, translation = root.checkpoint(3, 3, kept)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	var latest int64
	for event, err := range store.ExportEvents(ctx, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
		require.NoError(t, err)
		latest = max(latest, event.ID)
	}
	require.Greater(t, latest, manifest.HistoryEventID, "the relay's history must sit past the root boundary so the download refreshes the forward")

	forwarded, err := installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err, "a refresh must honor the superseded artifact offer exactly as installation did")
	require.Equal(t, manifest, forwarded.Manifest)
	require.Equal(t, device.Enrollment.RelayResetEpoch+1, forwarded.Translation.Authority.Epoch)
	retry, err := installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.Equal(t, forwarded, retry, "every download before activation rebuilds the same forward")
	reseeded, err := store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch), deviceBinding, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, reseeded, 1, "the new epoch reoffers the retained manifest")
	require.Equal(t, artifact.Digest, reseeded[0].SourceHash)
	still := unacknowledgedRelayOutbox(t, store, deviceBinding, device.Enrollment.RelayResetEpoch)
	require.Equal(t, owed, still, "the superseded offer stays in the old epoch as history")
}

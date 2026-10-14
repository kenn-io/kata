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

// RunRelayResetSupersedesArtifactOffers checks that an artifact manifest a
// downstream has not acknowledged does not block a root reset: the forward
// reseeds manifests for the new epoch from the installed projection, so the
// old offer is history like any other superseded delivery.
func RunRelayResetSupersedesArtifactOffers(t *testing.T, store db.Storage) {
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
	root := &rootResetFixture{t: t, project: project, pin: pin, private: rootPrivate, rootUID: rootUID, relayUID: store.InstanceUID(), binding: upstream}

	kept := root.created("Kept issue", 300, 1)
	purged := root.created("Purged at the root", 301, 2)
	manifest, snapshot, translation := root.checkpoint(2, 2, kept, purged)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "relay-member", AdminActor: "admin", PlaintextToken: "artifact-forward-parent-test-token"})
	require.NoError(t, err)
	device, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-forward-device-test-token"})
	require.NoError(t, err)
	deviceBinding := device.Enrollment.RelayBindingUID
	drainRelayGrant(t, store, deviceBinding, device.Enrollment.RelayResetEpoch)

	// Vectors for the kept issue arrive; their manifest is offered to the
	// device, which does not acknowledge it before the root resets.
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: kept.entity.EntityUID, ProducerInstanceUID: rootUID, Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText("Kept issue", ""), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)
	offered, err := store.PendingRelayDeliveries(ctx, deviceBinding, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, offered, 1, "the manifest is owed to the device")
	require.Equal(t, artifact.Digest, offered[0].SourceHash)
	// The relay pushes its own offers upstream before it asks for a reset.
	drainRelayGrant(t, store, upstream, 2)

	manifest, snapshot, translation = root.checkpoint(3, 3, kept)
	require.NoError(t, installer.InstallRelayReset(ctx, upstream, manifest, snapshot, translation), "an unacknowledged artifact offer is superseded, not a blocker")
	owed, err := store.PendingRelayDeliveries(ctx, deviceBinding, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Empty(t, owed, "the superseded offer is not repeated in the old epoch")
	record, found := supersededRecord(t, store, project.UID, deviceBinding)
	require.True(t, found)
	require.Equal(t, 1, record.Artifacts)
	forwarded, err := installer.CreateRelayReset(ctx, deviceBinding, db.RootAttributionSigner{})
	require.NoError(t, err)
	reseeded, err := store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, forwarded.Translation.Authority.Epoch), deviceBinding, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, reseeded, 1, "the new epoch reoffers the retained manifest from the installed projection")
	require.Equal(t, artifact.Digest, reseeded[0].SourceHash)
	require.Equal(t, offered[0].Body, reseeded[0].Body)
}

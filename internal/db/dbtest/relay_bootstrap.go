package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// RunRelayEnrollmentBootstrap exercises relay enrollment bootstrap on the supplied native store.
// R4: seeding crosses its bounded read batches, preserves signed receipts,
// and cannot change another child's already-emitted/acknowledged stream.
func RunRelayEnrollmentBootstrap(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "bootstrap-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "root-member")
	issue, _, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Seed all history", Author: "source-agent"})
	require.NoError(t, err)
	for range 101 {
		_, _, err = store.CreateComment(writeCtx, db.CreateCommentParams{IssueID: issue.ID, Author: "source-agent", Body: "Existing comment"})
		require.NoError(t, err)
	}
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "root-member", AdminActor: "admin", PlaintextToken: "bootstrap-parent-test-token"})
	require.NoError(t, err)
	create := func(peer, token string) db.CreatedFederationEnrollment {
		created, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: 1, Token: token})
		require.NoError(t, err)
		return created
	}
	first := create("00000000000000000000000003", "bootstrap-first-test-token")
	retained := make(map[string][]db.RelayEnvelope)
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		offers, err := store.PendingRelayDeliveries(ctx, first.Enrollment.RelayBindingUID, stream, 1024)
		require.NoError(t, err)
		require.Len(t, offers, 102)
		last := offers[len(offers)-1]
		require.NoError(t, store.AckRelayDeliveries(ctx, first.Enrollment.RelayBindingUID, 1, stream, last.Sequence, last.Digest))
		retained[stream] = offers
	}
	second := create("00000000000000000000000004", "bootstrap-second-test-token")
	require.Equal(t, second, create("00000000000000000000000004", "bootstrap-second-test-token"), "exact enrollment retry must retain its identities")
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt} {
		empty, err := store.PendingRelayDeliveries(ctx, first.Enrollment.RelayBindingUID, stream, 1024)
		require.NoError(t, err)
		require.Empty(t, empty, "new child cannot requeue an acknowledged sibling")
		offers, err := store.PendingRelayDeliveries(ctx, second.Enrollment.RelayBindingUID, stream, 1024)
		require.NoError(t, err)
		require.Len(t, offers, 102)
		replay, err := store.PendingRelayDeliveries(ctx, second.Enrollment.RelayBindingUID, stream, 1024)
		require.NoError(t, err)
		require.Equal(t, offers, replay)
		expected := retained[stream]
		if expected == nil {
			t.Fatal("missing retained source history for the relay stream")
			return
		}
		for i, offer := range offers {
			require.Equal(t, expected[i].SourceUID, offer.SourceUID)
			require.Equal(t, expected[i].SourceHash, offer.SourceHash)
			require.Equal(t, expected[i].Body, offer.Body)
			if stream == db.RelayStreamReceipt {
				var receipt db.AttributionReceipt
				require.NoError(t, json.Unmarshal(offer.Body, &receipt))
				require.NoError(t, db.VerifyRootReceipt(pin, receipt))
			}
		}
	}
}

// RunRelayEnrollmentBootstrapCompactedArtifacts exercises fresh enrollment
// when retained embedding manifests coexist with compacted source history.
// Reset-required enrollments must establish their signed checkpoint before
// seeding manifests into the new artifact epoch.
func RunRelayEnrollmentBootstrapCompactedArtifacts(t *testing.T, store db.Storage, compact func(context.Context, int64) error) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "compacted-bootstrap-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Portable vectors", Body: "Current issue content", Author: "source-agent"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)
	require.NoError(t, compact(ctx, project.ID))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "root-member", AdminActor: "admin", PlaintextToken: "compacted-bootstrap-parent-token"})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000005", ProtocolVersion: db.RelayProtocolVersion, Token: "compacted-bootstrap-child-token"})
	require.NoError(t, err)

	bootstrap := store.(db.RelayResetBootstrapStore)
	required, err := bootstrap.RelayEnrollmentNeedsReset(ctx, grant.Enrollment.RelayBindingUID)
	require.NoError(t, err)
	require.True(t, required, "compacted source history requires a signed snapshot")
	resetter := store.(interface {
		CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
	})
	checkpoint, err := resetter.CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, signer)
	require.NoError(t, err, "old-epoch manifests cannot block creation of the checkpoint needed to install them")
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	var snapshotManifests []embedding.ArtifactManifest
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Artifacts, &snapshotManifests, json.RejectUnknownMembers(true)))
	require.Equal(t, []embedding.ArtifactManifest{artifact.Manifest()}, snapshotManifests)

	installedCtx := db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch)
	offers, err := store.PendingRelayDeliveries(installedCtx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, offers, 1)
	require.Equal(t, checkpoint.Translation.Authority.Epoch, offers[0].Epoch, "manifest is emitted only in the checkpoint's new hop epoch")
	require.Equal(t, artifact.Digest, offers[0].SourceUID)
	require.Equal(t, artifact.Digest, offers[0].SourceHash, "the manifest is keyed by its exact artifact digest")
	var delivered embedding.ArtifactManifest
	require.NoError(t, json.Unmarshal(offers[0].Body, &delivered, json.RejectUnknownMembers(true)))
	require.Equal(t, artifact.Manifest(), delivered)
	require.NotContains(t, string(offers[0].Body), "vector_bytes\"", "artifact stream stays manifest-first")
}

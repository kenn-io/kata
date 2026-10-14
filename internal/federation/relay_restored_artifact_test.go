package federation_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// R4 supports soft-delete/restore; R7 requires reuse of retained compatible
// artifacts in both directions without new provider work.
func TestRelayRestoredArtifactDelivery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []string{"downstream", "upstream"} {
			t.Run(backend+"/"+direction, func(t *testing.T) {
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "personal-member")
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
				syncRelayMatrixNode(t, personal)
				sender, receiver := root, personal
				if direction == "upstream" {
					sender, receiver = personal, root
				}
				issue, _, err := sender.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.project.ID, Title: "Restored vector", Author: sender.account})
				require.NoError(t, err)
				identity := embedding.ArtifactIdentity{ProjectUID: root.project.UID, IssueUID: issue.UID, ProducerInstanceUID: sender.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
				require.NoError(t, err)
				durable, err := sender.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
				require.NoError(t, err)
				require.True(t, durable)
				binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				original, err := sender.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
				require.NoError(t, err)
				require.Len(t, original, 1)
				_, _, _, err = sender.store.SoftDeleteIssue(t.Context(), issue.ID, sender.account)
				require.NoError(t, err)
				syncRelayMatrixNode(t, personal)
				_, err = receiver.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, artifact.Digest)
				require.ErrorIs(t, err, db.ErrNotFound, "deletion correctly withheld artifact bytes")
				retained := restoredArtifactOutbox(t, sender.store, original[0].Sequence)
				for cycle := range 2 {
					_, event, changed, err := sender.store.RestoreIssue(t.Context(), issue.ID, sender.account)
					require.NoError(t, err)
					require.True(t, changed)
					require.NotNil(t, event)
					offered, err := sender.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
					require.NoError(t, err)
					require.Len(t, offered, 1, "restore must reoffer retained bytes without regenerating or re-retaining")
					require.Equal(t, artifact.Digest+":"+event.UID, offered[0].SourceUID)
					require.Equal(t, artifact.Digest, offered[0].SourceHash)
					require.Greater(t, offered[0].Sequence, original[0].Sequence)
					retry, err := sender.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
					require.NoError(t, err)
					require.Equal(t, offered, retry, "lost send/reply must keep restoration identity")
					syncRelayMatrixNode(t, personal)
					restored, err := receiver.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Nil(t, restored.DeletedAt)
					received, err := receiver.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, artifact.Digest)
					require.NoError(t, err, "restored content must regain compatible retained artifacts")
					require.Equal(t, artifact, received)
					replay, err := receiver.store.AcceptRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayBatch{Stream: db.RelayStreamArtifact, Envelopes: original})
					require.NoError(t, err)
					require.Empty(t, replay.MissingDigests, "an old retired retry must not downgrade its acceptance after restore")
					require.GreaterOrEqual(t, replay.Through, offered[0].Sequence)
					durable, err = sender.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
					require.NoError(t, err)
					require.True(t, durable)
					pending, err := sender.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
					require.NoError(t, err)
					require.Empty(t, pending, "repeat retention must not create another generation")
					require.Equal(t, retained, restoredArtifactOutbox(t, sender.store, original[0].Sequence), "old emitted envelope and acknowledgement remain immutable")
					if cycle == 0 {
						_, _, _, err = sender.store.SoftDeleteIssue(t.Context(), issue.ID, sender.account)
						require.NoError(t, err)
						syncRelayMatrixNode(t, personal)
					}
				}

			})
		}
	}
}

func restoredArtifactOutbox(t *testing.T, store db.Storage, sequence int64) db.RelayOutboxExport {
	t.Helper()
	for record, err := range store.ExportRelayState(t.Context()) {
		require.NoError(t, err)
		if outbox, ok := record.(*db.RelayOutboxExport); ok && outbox.ID == sequence {
			return *outbox
		}
	}
	t.Fatal("original artifact outbox record disappeared")
	return db.RelayOutboxExport{}
}

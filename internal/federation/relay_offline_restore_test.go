package federation_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// R4 permits offline ordinary delete/restore; R7 retained bytes must resume in either direction.
func TestRelayOfflineRestoreArtifactDelivery(t *testing.T) {
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
				issue, _, err := sender.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.project.ID, Title: "Offline restored vector", Author: sender.account})
				require.NoError(t, err)
				identity := embedding.ArtifactIdentity{ProjectUID: root.project.UID, IssueUID: issue.UID, ProducerInstanceUID: sender.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
				require.NoError(t, err)
				durable, err := sender.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
				require.NoError(t, err)
				require.True(t, durable)
				_, _, _, err = sender.store.SoftDeleteIssue(t.Context(), issue.ID, sender.account)
				require.NoError(t, err)
				_, _, changed, err := sender.store.RestoreIssue(t.Context(), issue.ID, sender.account)
				require.NoError(t, err)
				require.True(t, changed)
				binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				offers, err := sender.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
				require.NoError(t, err)
				require.Len(t, offers, 2)
				require.NotEqual(t, offers[0].SourceUID, offers[1].SourceUID)
				require.Equal(t, artifact.Digest, offers[0].SourceHash)
				require.Equal(t, artifact.Digest, offers[1].SourceHash)
				retry, err := sender.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
				require.NoError(t, err)
				require.Equal(t, offers, retry, "offline retry preserves both emitted offer identities")
				syncRelayMatrixNode(t, personal)
				got, err := receiver.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, artifact.Digest)
				require.NoError(t, err)
				require.Equal(t, artifact, got)
				pending, err := sender.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
				require.NoError(t, err)
				require.Empty(t, pending, "one exact payload must settle both emitted offers")
				syncRelayMatrixNode(t, personal)
				received, err := receiver.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, artifact.Digest)
				require.NoError(t, err)
				require.Equal(t, artifact, received, "replay preserves the retained artifact")
			})
		}
	}
}

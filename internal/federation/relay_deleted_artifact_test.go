package federation_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
)

func TestRelayDeletedArtifactPrefix(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRecoveryRoot(t, backend)
			personal := newRelayMatrixNode(t, backend, "personal-member")
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			syncRelayMatrixNode(t, personal)
			a, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: root.project.ID, Title: "Deleted vector", Author: root.account})
			require.NoError(t, err)
			identity := embedding.ArtifactIdentity{ProjectUID: root.project.UID, IssueUID: a.UID, ProducerInstanceUID: root.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(a.Title, a.Body), [][]float32{{1, 0}})
			require.NoError(t, err)
			durable, err := root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
			require.NoError(t, err)
			require.True(t, durable)
			_, _, _, err = root.store.SoftDeleteIssue(t.Context(), a.ID, root.account)
			require.NoError(t, err)
			b, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: root.project.ID, Title: "Live vector", Author: root.account})
			require.NoError(t, err)
			identity.IssueUID = b.UID
			second, err := embedding.NewArtifact(identity, embedding.EmbedText(b.Title, b.Body), [][]float32{{0, 1}})
			require.NoError(t, err)
			durable, err = root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), second)
			require.NoError(t, err)
			require.True(t, durable)
			syncRelayMatrixNode(t, personal)
			syncRelayMatrixNode(t, personal)
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			pending, err := root.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
			require.NoError(t, err)
			require.Empty(t, pending, "the deleted issue's artifact must not permanently block the accepted prefix or later valid artifacts")
			_, err = personal.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, artifact.Digest)
			require.ErrorIs(t, err, db.ErrNotFound, "deleted content must not receive new vector bytes")
			retained, err := personal.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, second.Digest)
			require.NoError(t, err)
			require.Equal(t, second, retained)
			deleted, err := personal.store.IssueByUID(t.Context(), a.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.NotNil(t, deleted.DeletedAt, "retaining bytes does not restore the issue")
		})
	}
}

// R7: a prior manifest is not permission to download or retain deleted vectors.
func TestRelayDeletedArtifactInFlight(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []string{"downstream", "upstream"} {
			t.Run(backend+"/"+direction, func(t *testing.T) {
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "personal-member")
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
				syncRelayMatrixNode(t, personal)
				issue, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: root.project.ID, Title: "In flight vectors", Author: root.account})
				require.NoError(t, err)
				syncRelayMatrixNode(t, personal)
				sender, receiver := root, personal
				if direction == "upstream" {
					sender, receiver = personal, root
				}
				identity := embedding.ArtifactIdentity{ProjectUID: root.project.UID, IssueUID: issue.UID, ProducerInstanceUID: sender.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
				require.NoError(t, err)
				durable, err := sender.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
				require.NoError(t, err)
				require.True(t, durable)
				binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				c := binding.RelayConfig
				offered, err := sender.store.PendingRelayDeliveries(t.Context(), c.BindingUID, db.RelayStreamArtifact, 32)
				require.NoError(t, err)
				require.Len(t, offered, 1)
				batch := db.RelayBatch{Stream: db.RelayStreamArtifact, Envelopes: offered}
				acceptance, err := receiver.store.AcceptRelayDeliveries(t.Context(), c.BindingUID, batch)
				require.NoError(t, err)
				require.Equal(t, []string{artifact.Digest}, acceptance.MissingDigests)
				require.Zero(t, acceptance.Through)
				payloads, err := sender.store.(db.RelayArtifactStorage).RelayArtifactPayloads(t.Context(), c.BindingUID, c.ResetEpoch, acceptance.MissingDigests)
				require.NoError(t, err)
				require.Len(t, payloads, 1)
				senderIssue, err := sender.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				_, _, _, err = sender.store.SoftDeleteIssue(t.Context(), senderIssue.ID, sender.account)
				require.NoError(t, err)
				payloadsAfterDelete, err := sender.store.(db.RelayArtifactStorage).RelayArtifactPayloads(t.Context(), c.BindingUID, c.ResetEpoch, acceptance.MissingDigests)
				require.ErrorIs(t, err, db.ErrEmbeddingArtifactMiss, "retired downloads are retryable, not authority revocation")
				require.Empty(t, payloadsAfterDelete)
				if direction == "downstream" {
					remote, err := federation.NewClient(t.Context(), root.http.URL, personal.credential.Token, client.Opts{})
					require.NoError(t, err)
					remotePayload, err := remote.DownloadRelayArtifacts(t.Context(), root.project.ID, c.ResetEpoch, acceptance.MissingDigests)
					var statusErr *federation.HubStatusError
					require.ErrorAs(t, err, &statusErr)
					require.Equal(t, http.StatusConflict, statusErr.StatusCode, "artifact retirement must remain a retryable transport miss")
					require.Contains(t, statusErr.Body, "artifact_miss")
					require.Empty(t, remotePayload.Artifacts)
				}
				receiverIssue, err := receiver.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				_, _, _, err = receiver.store.SoftDeleteIssue(t.Context(), receiverIssue.ID, receiver.account)
				require.NoError(t, err)
				batch.Artifacts = payloads // Download won the race; commit must still discard it.
				acceptance, err = receiver.store.AcceptRelayDeliveries(t.Context(), c.BindingUID, batch)
				require.NoError(t, err)
				require.Empty(t, acceptance.MissingDigests, "a tombstone retires the offer rather than requesting vectors forever")
				require.Equal(t, offered[0].Sequence, acceptance.Through)
				require.Equal(t, offered[0].Digest, acceptance.Digest)
				require.NoError(t, sender.store.AckRelayDeliveries(t.Context(), c.BindingUID, c.ResetEpoch, db.RelayStreamArtifact, acceptance.Through, acceptance.Digest))
				_, err = receiver.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, artifact.Digest)
				require.ErrorIs(t, err, db.ErrNotFound)
				pending, err := sender.store.PendingRelayDeliveries(t.Context(), c.BindingUID, db.RelayStreamArtifact, 32)
				require.NoError(t, err)
				require.Empty(t, pending)
				// Lost acceptance replies replay the original hop identity;
				// retirement remains terminal and never becomes a vector hit.
				replay, err := receiver.store.AcceptRelayDeliveries(t.Context(), c.BindingUID, batch)
				require.NoError(t, err)
				require.Equal(t, acceptance, replay)
				durable, err = receiver.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
				require.ErrorIs(t, err, db.ErrEmbeddingArtifactMiss)
				require.False(t, durable)
				original, err := sender.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), root.project.UID, artifact.Digest)
				require.NoError(t, err)
				require.Equal(t, artifact, original, "retirement preserves the sender's portable backup bytes")
			})
		}
	}
}

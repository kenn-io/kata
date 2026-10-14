package federation_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// A replica prunes an issue's artifact once it retains the artifact for newer
// content. When the issue reverts to that earlier content, the root's artifact
// has the same digest as the acknowledged offer, and the replica must still
// receive it again.
func TestRelayArtifactRevertIsOfferedAgain(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member", true)
			relay := newRelayMatrixNode(t, backend, "relay-member", true)
			project, err := root.store.CreateProject(ctx, "reverted-artifact-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			enrollRelayMatrixReplica(t, root, relay, "reverted-relay", false)
			signed := db.WithRootAttribution(ctx, root.signer, root.account)
			issue, _, err := root.store.CreateIssue(signed, db.CreateIssueParams{ProjectID: project.ID, Title: "Content A", Author: "assistant"})
			require.NoError(t, err)
			client, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:1", Model: "example-model", Dims: 2})
			require.NoError(t, err)
			identity, err := client.ArtifactIdentity(project.UID, issue.UID, root.store.InstanceUID())
			require.NoError(t, err)
			retain := func(title string) embedding.EmbeddingArtifact {
				t.Helper()
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(title, ""), [][]float32{{1, 0}})
				require.NoError(t, err)
				durable, err := root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
				require.NoError(t, err)
				require.True(t, durable)
				return artifact
			}
			edit := func(title string) {
				t.Helper()
				_, _, _, err := root.store.EditIssue(signed, db.EditIssueParams{IssueID: issue.ID, Actor: "assistant", Title: &title})
				require.NoError(t, err)
			}
			relayArtifacts := relay.store.(db.EmbeddingArtifactStorage)

			first := retain("Content A")
			syncRelayMatrixNode(t, relay)
			_, err = relayArtifacts.StoredEmbeddingArtifact(ctx, relay.project.UID, first.Digest)
			require.NoError(t, err)
			edit("Content B")
			second := retain("Content B")
			syncRelayMatrixNode(t, relay)
			_, err = relayArtifacts.StoredEmbeddingArtifact(ctx, relay.project.UID, second.Digest)
			require.NoError(t, err)
			_, err = relayArtifacts.StoredEmbeddingArtifact(ctx, relay.project.UID, first.Digest)
			require.ErrorIs(t, err, db.ErrNotFound, "the replica pruned the artifact for older content")

			edit("Content A")
			reverted := retain("Content A")
			require.Equal(t, first.Digest, reverted.Digest)
			syncRelayMatrixNode(t, relay)
			_, err = relayArtifacts.StoredEmbeddingArtifact(ctx, relay.project.UID, first.Digest)
			require.NoError(t, err, "the replica receives the reverted content's artifact again")
		})
	}
}

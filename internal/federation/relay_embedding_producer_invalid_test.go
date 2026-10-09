package federation_test

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
)

// R7: malformed configuration retained by an older backup defers embedding
// generation while ordinary relay enrollment and issue replication continue.
func TestMalformedProducerRelayContinues(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"existing_enrollment", "new_enrollment"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member")
				relay := newRelayMatrixNode(t, backend, "relay-member")
				project, err := root.store.CreateProject(ctx, "invalid-producer-project")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				if mode == "existing_enrollment" {
					enrollRelayMatrixReplica(t, root, relay, "existing-relay", true)
				}
				client, err := embedding.New(embedding.Config{BaseURL: "https://embedding.example", Model: "example-model", Dims: 2})
				require.NoError(t, err)
				recipe, err := client.ArtifactIdentity("", "", "")
				require.NoError(t, err)
				recipe.Dimensions = 0
				raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: root.store.InstanceUID(), Recipe: recipe})
				require.NoError(t, err)
				metadata, err := json.Marshal(map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw})
				require.NoError(t, err)
				// This seeds the explicit retained-backup contract; normal patches reject it.
				if pg, ok := root.store.(*pgstore.Store); ok {
					_, err = pg.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(metadata), project.ID)
				} else {
					_, err = root.store.(*sqlitestore.Store).ExecContext(ctx, `UPDATE projects SET metadata=? WHERE id=?`, string(metadata), project.ID)
				}
				require.NoError(t, err)
				issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Content survives invalid embedding configuration", Author: "assistant"})
				require.NoError(t, err)
				if mode == "new_enrollment" {
					enrollRelayMatrixReplica(t, root, relay, "new-relay", true)
				}
				syncRelayMatrixNode(t, relay)
				received, err := relay.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, issue.Title, received.Title)
			})
		}
	}
}

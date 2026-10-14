package federation_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// A10/A12: complete original artifacts from each role traverse the actual
// authenticated transport and answer semantic queries on all eight native
// backend combinations. This proves reuse; stable producer selection is separate.
func TestEmbeddingArtifactRelayReuseBackendMatrix(t *testing.T) {
	for _, rootBackend := range []string{"sqlite", "postgres"} {
		for _, relayBackend := range []string{"sqlite", "postgres"} {
			for _, leafBackend := range []string{"sqlite", "postgres"} {
				t.Run(fmt.Sprintf("%s_%s_%s", rootBackend, relayBackend, leafBackend), func(t *testing.T) {
					ctx := t.Context()
					root := newRelayMatrixNode(t, rootBackend, "root-member", true)
					personal := newRelayMatrixNode(t, relayBackend, "relay-member", true)
					second := newRelayMatrixNode(t, relayBackend, "second-member", true)
					leaf := newRelayMatrixNode(t, leafBackend, "leaf-member", true)
					secondLeaf := newRelayMatrixNode(t, leafBackend, "second-leaf-member", true)
					project, err := root.store.CreateProject(ctx, "shared-artifact-matrix")
					require.NoError(t, err)
					root.project = project
					_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
					require.NoError(t, err)
					public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
					require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
					enrollRelayMatrixReplica(t, root, personal, "relay-alias", true)
					enrollRelayMatrixReplica(t, root, second, "second-alias", true)
					enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
					enrollRelayMatrixReplica(t, personal, secondLeaf, "second-leaf-alias", false)
					issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Received complete manifest chunks", Body: strings.Repeat("界", 2500), Author: "assistant"})
					require.NoError(t, err)
					nodes := []*relayMatrixNode{root, personal, second, leaf, secondLeaf}
					for _, node := range nodes[1:] {
						syncRelayMatrixNode(t, node)
					}
					var documentRequests, queryRequests atomic.Int32
					provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var request struct {
							Input []string `json:"input"`
						}
						require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
						query := len(request.Input) == 1 && request.Input[0] == "received second chunk query"
						if query {
							queryRequests.Add(1)
						} else {
							documentRequests.Add(1)
						}
						data := []map[string]any{}
						for i := range request.Input {
							data = append(data, map[string]any{"index": i, "embedding": []float32{0, 1}})
						}
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
					}))
					defer provider.Close()
					client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
					require.NoError(t, err)
					artifacts := []embedding.EmbeddingArtifact{}
					for _, origin := range []*relayMatrixNode{root, personal, leaf} {
						identity, err := client.ArtifactIdentity(project.UID, issue.UID, origin.store.InstanceUID())
						require.NoError(t, err)
						artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}, {0, 1}})
						require.NoError(t, err)
						durable, err := origin.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
						require.NoError(t, err)
						require.True(t, durable)
						artifacts = append(artifacts, artifact)
					}
					for range 3 {
						for _, node := range nodes[1:] {
							syncRelayMatrixNode(t, node)
						}
					}
					for _, node := range nodes {
						for _, artifact := range artifacts {
							got, err := node.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
							require.NoError(t, err, "every origin's exact complete artifact reaches every node")
							require.Equal(t, artifact, got)
						}
						var ix *vector.Index
						if pg, ok := node.store.(*pgstore.Store); ok {
							ix, err = vector.OpenPostgres(ctx, pg.DB)
						} else {
							ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
						}
						require.NoError(t, err)
						reconciler := daemon.NewReconciler(node.store, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour})
						runCtx, cancel := context.WithCancel(ctx)
						done := make(chan error, 1)
						t.Cleanup(cancel)
						go func() { done <- reconciler.Run(runCtx) }()
						//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
						require.Eventually(t, func() bool { return reconciler.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
						cancel()
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Fatal("embedding worker did not stop")
						}
						health := reconciler.Health()
						require.EqualValues(t, 1, health.Embedded)
						require.Zero(t, health.Backlog)
						query, err := client.Embed(ctx, []string{"received second chunk query"})
						require.NoError(t, err)
						hits, err := ix.Query(db.WithAuthorizedProjects(ctx, []string{project.UID}), client.Generation().Fingerprint(), kitvec.Vector(query[0]), 1)
						require.NoError(t, err)
						require.Len(t, hits, 1)
						require.Equal(t, issue.UID, hits[0].Doc)
						require.Equal(t, 1, hits[0].ChunkIndex)
						require.NoError(t, ix.Close())
					}
					require.Zero(t, documentRequests.Load(), "received artifacts suppress document HTTP requests at all five nodes")
					require.EqualValues(t, 5, queryRequests.Load())
				})
			}
		}
	}
}

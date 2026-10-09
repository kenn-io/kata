package federation_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// A10/A12: five simultaneously running existing workers on all eight backend
// combinations use only the stable root/relay producer. Each fixture incurs
// one successful local request; received vectors answer real semantic queries.
func TestEmbeddingProjectSingleProducerBackendMatrix(t *testing.T) {
	for _, rootBackend := range []string{"sqlite", "postgres"} {
		for _, relayBackend := range []string{"sqlite", "postgres"} {
			for _, leafBackend := range []string{"sqlite", "postgres"} {
				for _, producerRole := range []string{"root", "relay"} {
					t.Run(fmt.Sprintf("%s_%s_%s/%s", rootBackend, relayBackend, leafBackend, producerRole), func(t *testing.T) {
						ctx := t.Context()
						root := newRelayMatrixNode(t, rootBackend, "root-member", true)
						relay := newRelayMatrixNode(t, relayBackend, "relay-member", true)
						second := newRelayMatrixNode(t, relayBackend, "second-member", true)
						leaf := newRelayMatrixNode(t, leafBackend, "leaf-member", true)
						secondLeaf := newRelayMatrixNode(t, leafBackend, "second-leaf-member", true)
						nodes := []*relayMatrixNode{root, relay, second, leaf, secondLeaf}
						project, err := root.store.CreateProject(ctx, "single-producer-matrix")
						require.NoError(t, err)
						root.project = project
						_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
						require.NoError(t, err)
						public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
						require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
						var documents, inputs, queries [5]atomic.Int64
						provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							n, err := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer matrix-provider-"))
							require.NoError(t, err)
							require.True(t, n >= 0 && n < len(nodes))
							var request struct {
								Input []string `json:"input"`
							}
							require.NoError(t, json.UnmarshalRead(r.Body, &request))
							query := len(request.Input) == 1 && request.Input[0] == "received second chunk query"
							if query {
								queries[n].Add(1)
							} else {
								documents[n].Add(1)
								inputs[n].Add(int64(len(request.Input)))
							}
							data := []map[string]any{}
							for i, text := range request.Input {
								value := []float32{1, 0}
								if query || strings.HasPrefix(text, "界") {
									value = []float32{0, 1}
								}
								data = append(data, map[string]any{"index": i, "embedding": value})
							}
							require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
						}))
						t.Cleanup(provider.Close)
						clients := make([]*embedding.Client, len(nodes))
						for i := range nodes {
							clients[i], err = embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2, Credential: config.EmbeddingCredential{Key: fmt.Sprintf("matrix-provider-%d", i)}})
							require.NoError(t, err)
						}
						producerIndex := 0
						if producerRole == "relay" {
							producerIndex = 1
						}
						recipe, err := clients[producerIndex].ArtifactIdentity("", "", "")
						require.NoError(t, err)
						producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: nodes[producerIndex].store.InstanceUID(), Recipe: recipe}
						selectProducer := func(producer db.ProjectEmbeddingProducer) {
							raw, err := json.Marshal(producer)
							require.NoError(t, err)
							_, err = root.store.PatchProjectMetadata(db.WithRootAttribution(ctx, root.signer, root.account), db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: root.account, Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
							require.NoError(t, err)
						}
						selectProducer(producer)
						enrollRelayMatrixReplica(t, root, relay, "producer-relay", true)
						enrollRelayMatrixReplica(t, root, second, "producer-second-relay", true)
						enrollRelayMatrixReplica(t, relay, leaf, "producer-leaf", false)
						enrollRelayMatrixReplica(t, relay, secondLeaf, "producer-second-leaf", false)
						syncAll := func() {
							for range 3 {
								for _, node := range nodes[1:] {
									syncRelayMatrixNode(t, node)
								}
							}
						}
						first, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Single shared chunk", Author: "assistant"})
						require.NoError(t, err)
						syncAll()
						indexes := make([]*vector.Index, len(nodes))
						workers := make([]*daemon.Reconciler, len(nodes))
						stops := make([]func(), len(nodes))
						startWorker := func(i int, gate <-chan struct{}) {
							node := nodes[i]
							workers[i] = daemon.NewReconciler(node.store, indexes[i], clients[i], daemon.ReconcilerConfig{SweepEvery: time.Hour, FederationProducerAllowed: func(ctx context.Context, uid string, recipe embedding.ArtifactIdentity, _ activity.Admission, syncArtifacts bool) (bool, error) {
								if !syncArtifacts {
									return federation.EmbeddingProducerAuthorized(ctx, node.store, node.credentials, uid, recipe)
								}
								return federation.EmbeddingProducerConnected(ctx, node.store, node.credentials, uid, recipe)
							}})
							runCtx, cancel := context.WithCancel(ctx)
							done := make(chan error, 1)
							go func() {
								if gate != nil {
									<-gate
								}
								done <- workers[i].Run(runCtx)
							}()
							var once sync.Once
							stop := func() {
								once.Do(func() {
									cancel()
									select {
									case <-done:
									case <-time.After(10 * time.Second):
										t.Error("matrix worker did not stop")
									}
								})
							}
							stops[i] = stop
							t.Cleanup(stop)
						}
						for i, node := range nodes {
							if pg, ok := node.store.(*pgstore.Store); ok {
								indexes[i], err = vector.OpenPostgres(ctx, pg.DB)
							} else {
								indexes[i], err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
							}
							require.NoError(t, err)
							ix := indexes[i]
							t.Cleanup(func() { require.NoError(t, ix.Close()) })
						}
						// Start every worker before observing the first successful generation.
						gate := make(chan struct{})
						for i := range nodes {
							startWorker(i, gate)
						}
						close(gate)
						waitProducer := func(expected int64) {
							//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
							require.Eventually(t, func() bool {
								return workers[producerIndex].Health().LastSuccessAt != nil && documents[producerIndex].Load() == expected && workers[producerIndex].Health().Backlog == 0
							}, 30*time.Second, 10*time.Millisecond)
						}
						waitAll := func(embedded int64) {
							syncAll()
							for _, worker := range workers {
								worker.Wake()
							}
							for _, worker := range workers {
								//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
								require.Eventually(t, func() bool {
									h := worker.Health()
									return h.LastSuccessAt != nil && h.Backlog == 0 && h.Embedded == embedded
								}, 30*time.Second, 10*time.Millisecond)
							}
						}
						checkSpend := func(expected int64) {
							for i := range nodes {
								if i == producerIndex {
									require.Equal(t, expected, documents[i].Load())
								} else {
									require.Zero(t, documents[i].Load(), "recipient must not generate shared document vectors")
								}
							}
						}
						waitProducer(1)
						waitAll(1)
						checkSpend(1)
						require.EqualValues(t, 1, inputs[producerIndex].Load(), "single chunk uses one local worker request")
						secondIssue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Complete shared chunks", Body: strings.Repeat("界", 2500), Author: "assistant"})
						require.NoError(t, err)
						syncAll()
						for _, worker := range workers {
							worker.Wake()
						}
						waitProducer(2)
						waitAll(2)
						checkSpend(2)
						require.EqualValues(t, 3, inputs[producerIndex].Load(), "multichunk baseline adds one request containing both chunks")
						for i, node := range nodes {
							manifests, err := node.store.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, project.UID, 10)
							require.NoError(t, err)
							require.Len(t, manifests, 2)
							for _, manifest := range manifests {
								require.Equal(t, producer.ProducerInstanceUID, manifest.ProducerInstanceUID)
								require.True(t, manifest.IssueUID == first.UID || manifest.IssueUID == secondIssue.UID)
							}
							query, err := clients[i].Embed(ctx, []string{"received second chunk query"})
							require.NoError(t, err)
							hits, err := indexes[i].Query(db.WithAuthorizedProjects(ctx, []string{project.UID}), clients[i].Generation().Fingerprint(), kitvec.Vector(query[0]), 1)
							require.NoError(t, err)
							require.Len(t, hits, 1)
							require.Equal(t, secondIssue.UID, hits[0].Doc)
							require.Equal(t, 1, hits[0].ChunkIndex)
							require.EqualValues(t, 1, queries[i].Load())
						}
						_, err = root.store.PatchProjectMetadata(db.WithRootAttribution(ctx, root.signer, root.account), db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: root.account, Patch: map[string]jsontext.Value{"area": jsontext.Value(`"example-area"`)}})
						require.NoError(t, err)
						waitAll(2)
						checkSpend(2)
						// Explicit reassignment preserves already valid artifacts; no automatic
						// failover is exercised or promised across ambiguous provider responses.
						changed := producer
						changed.ProducerInstanceUID = second.store.InstanceUID()
						selectProducer(changed)
						waitAll(2)
						checkSpend(2)
						for _, stop := range stops {
							stop()
						}
						for i := range nodes {
							startWorker(i, nil)
						}
						for _, worker := range workers {
							//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
							require.Eventually(t, func() bool { return worker.Health().LastSuccessAt != nil && worker.Health().Backlog == 0 }, 30*time.Second, 10*time.Millisecond)
						}
						checkSpend(2)
					})
				}
			}
		}
	}
}

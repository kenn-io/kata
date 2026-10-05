package federation_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// R7: a root-selected connected relay may generate the preferred recipe using
// its existing local worker; the root remains a nonproducer and does not spend.
func TestEmbeddingSelectedRelayProducer(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"online", "enrollment_pending", "unreachable", "wrong_origin", "parent_revoked", "local_membership_revoked", "reconnect", "reconnect_with_artifact", "reconnect_unsynced_artifact"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member", true)
				var connected atomic.Bool
				connected.Store(true)
				originalHandler := root.http.Config.Handler
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !connected.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					originalHandler.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				root.http = proxy
				relay := newRelayMatrixNode(t, backend, "relay-member", true)
				project, err := root.store.CreateProject(ctx, "selected-relay-producer")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				var requests atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var input struct {
						Input []string `json:"input"`
					}
					require.NoError(t, json.UnmarshalRead(r.Body, &input))
					require.Len(t, input.Input, 2)
					require.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0}}, {"index": 1, "embedding": []float32{0, 1}}}}))
				}))
				t.Cleanup(provider.Close)
				client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
				require.NoError(t, err)
				recipe, err := client.ArtifactIdentity("", "", "")
				require.NoError(t, err)
				producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: relay.store.InstanceUID(), Recipe: recipe}
				raw, err := json.Marshal(producer)
				require.NoError(t, err)
				_, err = root.store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
				require.NoError(t, err)
				enrollRelayMatrixReplica(t, root, relay, "relay-producer-alias", true)
				issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Relay generated chunks", Body: strings.Repeat("界", 2100), Author: "assistant"})
				require.NoError(t, err)
				syncRelayMatrixNode(t, relay)
				current, err := relay.store.ProjectByID(ctx, relay.project.ID)
				require.NoError(t, err)
				selected, err := db.ProjectEmbeddingProducerFromMetadata(current.Metadata)
				require.NoError(t, err)
				require.NotNil(t, selected)
				require.Equal(t, producer, *selected)
				if mode == "reconnect_with_artifact" || mode == "reconnect_unsynced_artifact" {
					identity, err := client.ArtifactIdentity(project.UID, issue.UID, root.store.InstanceUID())
					require.NoError(t, err)
					artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}, {0, 1}})
					require.NoError(t, err)
					durable, err := root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
					require.NoError(t, err)
					require.True(t, durable)
				}

				var canaryRequests atomic.Int32
				canary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					canaryRequests.Add(1)
					w.WriteHeader(http.StatusForbidden)
				}))
				t.Cleanup(canary.Close)
				if mode == "unreachable" || mode == "reconnect" || (mode == "reconnect_with_artifact" || mode == "reconnect_unsynced_artifact") {
					connected.Store(false)
				}
				if mode == "wrong_origin" {
					misrouted := relay.credential
					misrouted.HubURL = canary.URL
					require.NoError(t, relay.credentials.StoreFederationCredential(ctx, relay.project.UID, misrouted))
				}
				if mode == "enrollment_pending" {
					pending := relay.credential
					pending.RelayEnrollmentPending = true
					require.NoError(t, relay.credentials.StoreFederationCredential(ctx, relay.project.UID, pending))
				}
				if mode == "parent_revoked" {
					parent, err := root.store.ResolveAPIToken(ctx, root.userToken)
					require.NoError(t, err)
					_, _, err = root.store.RevokeAPIToken(ctx, parent.ID, "owner")
					require.NoError(t, err)
				}
				if mode == "local_membership_revoked" {
					team, _, err := relay.store.CreateTeam(ctx, "producer-allowed-team", "owner")
					require.NoError(t, err)
					_, err = relay.store.SetTeamMembership(ctx, team.UID, relay.account, true, "owner")
					require.NoError(t, err)
					policy, err := relay.store.ProjectAccessPolicy(ctx, relay.project.UID)
					require.NoError(t, err)
					policy.Visibility = "teams"
					policy.TeamUIDs = []string{team.UID}
					_, _, err = relay.store.SetProjectAccessPolicy(ctx, policy, "owner")
					require.NoError(t, err)
					_, err = relay.store.SetTeamMembership(ctx, team.UID, relay.account, false, "owner")
					require.NoError(t, err)
				}

				for _, node := range []*relayMatrixNode{root, relay} {
					var ix *vector.Index
					if pg, ok := node.store.(*pgstore.Store); ok {
						ix, err = vector.OpenPostgres(ctx, pg.DB)
					} else {
						ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
					}
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, ix.Close()) })
					worker := daemon.NewReconciler(node.store, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour, FederationProducerAllowed: func(ctx context.Context, uid string, recipe embedding.ArtifactIdentity, _ activity.Admission, syncArtifacts bool) (bool, error) {
						if !syncArtifacts {
							return federation.EmbeddingProducerAuthorized(ctx, node.store, node.credentials, uid, recipe)
						}
						return federation.EmbeddingProducerConnected(ctx, node.store, node.credentials, uid, recipe)
					}})
					runCtx, cancel := context.WithCancel(ctx)
					done := make(chan error, 1)
					go func() { done <- worker.Run(runCtx) }()
					//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
					require.Eventually(t, func() bool { return worker.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
					cancel()
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Fatal("producer worker did not stop")
					}
					if node == root {
						require.Zero(t, requests.Load())
					} else if mode == "online" {
						require.EqualValues(t, 1, requests.Load(), "selected connected relay uses one local worker request")
					} else {
						require.Zero(t, requests.Load(), "selected relay requires current authorized upstream access")
						require.EqualValues(t, 1, worker.Health().Backlog)
						if mode == "reconnect" || (mode == "reconnect_with_artifact" || mode == "reconnect_unsynced_artifact") {
							connected.Store(true)
							if mode != "reconnect_unsynced_artifact" {
								syncRelayMatrixNode(t, relay)
							}
							replay := daemon.NewReconciler(node.store, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour, FederationProducerAllowed: func(ctx context.Context, uid string, recipe embedding.ArtifactIdentity, _ activity.Admission, syncArtifacts bool) (bool, error) {
								if !syncArtifacts {
									return federation.EmbeddingProducerAuthorized(ctx, node.store, node.credentials, uid, recipe)
								}
								return federation.EmbeddingProducerConnected(ctx, node.store, node.credentials, uid, recipe)
							}})
							replayCtx, stop := context.WithCancel(ctx)
							t.Cleanup(stop)
							finished := make(chan error, 1)
							go func() { finished <- replay.Run(replayCtx) }()
							//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
							require.Eventually(t, func() bool { return replay.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
							stop()
							select {
							case <-finished:
							case <-time.After(10 * time.Second):
								t.Fatal("reconnected producer worker did not stop")
							}
							if mode == "reconnect_with_artifact" || mode == "reconnect_unsynced_artifact" {
								require.Zero(t, requests.Load(), "reconnect downloads compatible artifacts before paid fill even at the designated relay")
								require.Zero(t, replay.Health().Backlog, "downloaded artifacts must enter the index before fill finishes")
								hits, err := ix.Query(db.WithAuthorizedProjects(ctx, []string{project.UID}), client.Generation().Fingerprint(), kitvec.Vector{0, 1}, 1)
								require.NoError(t, err)
								require.Len(t, hits, 1)
								require.Equal(t, issue.UID, hits[0].Doc)
								require.Equal(t, 1, hits[0].ChunkIndex)
							} else {
								require.EqualValues(t, 1, requests.Load(), "reconnection resumes the same designated local worker")
							}
						}
					}
					require.Zero(t, canaryRequests.Load(), "enrollment credentials stay on their configured origin")
				}
			})
		}
	}
}

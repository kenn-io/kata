package federation_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// R7: vector-only transport must wake the existing waiting local worker after
// durable commit, without an issue event or waiting for the hourly sweep.
func TestEmbeddingArtifactOnlyArrivalWakesWorker(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []string{"push", "pull", "pull_lost_ack"} {
			t.Run(backend+"/"+direction, func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member", true)
				relay := newRelayMatrixNode(t, backend, "relay-member", true)
				project, err := root.store.CreateProject(ctx, "artifact-wake-project")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				var calls atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				t.Cleanup(provider.Close)
				client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
				require.NoError(t, err)
				recipe, err := client.ArtifactIdentity("", "", "")
				require.NoError(t, err)
				receiver, origin := root, relay
				if direction != "push" {
					receiver, origin = relay, root
				}
				raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: origin.store.InstanceUID(), Recipe: recipe})
				require.NoError(t, err)
				_, err = root.store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
				require.NoError(t, err)
				var ix *vector.Index
				if pg, ok := receiver.store.(*pgstore.Store); ok {
					ix, err = vector.OpenPostgres(ctx, pg.DB)
				} else {
					ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				}
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, ix.Close()) })
				worker := daemon.NewReconciler(receiver.store, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour})
				if direction == "push" {
					server := daemon.NewServer(daemon.ServerConfig{DB: root.store, RootAttributionSigner: &root.signer, FederationCredentials: root.credentials, EmbeddingWake: worker.RetryNow, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
					t.Cleanup(func() { require.NoError(t, server.Close()) })
					root.http = httptest.NewServer(server.Handler())
					t.Cleanup(root.http.Close)
				}
				var loseAck atomic.Bool
				if direction == "pull_lost_ack" {
					handler := root.http.Config.Handler
					root.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/relay:ack") && loseAck.Load() {
							raw, err := io.ReadAll(r.Body)
							require.NoError(t, err)
							r.Body = io.NopCloser(bytes.NewReader(raw))
							var ack struct {
								Stream string `json:"stream"`
							}
							require.NoError(t, json.Unmarshal(raw, &ack))
							if ack.Stream == db.RelayStreamArtifact {
								w.WriteHeader(http.StatusServiceUnavailable)
								return
							}
						}
						handler.ServeHTTP(w, r)
					}))
					t.Cleanup(root.http.Close)
				}
				enrollRelayMatrixReplica(t, root, relay, "wake-relay", true)
				issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Vector only notification", Body: strings.Repeat("界", 2100), Author: "assistant"})
				require.NoError(t, err)
				syncRelayMatrixNode(t, relay)
				runCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- worker.Run(runCtx) }()
				defer func() {
					cancel()
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("artifact worker did not stop")
					}
				}()
				//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
				require.Eventually(t, func() bool { return worker.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
				require.EqualValues(t, 1, worker.Health().Backlog)
				require.Zero(t, calls.Load())
				identity, err := client.ArtifactIdentity(project.UID, issue.UID, origin.store.InstanceUID())
				require.NoError(t, err)
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}, {0, 1}})
				require.NoError(t, err)
				durable, err := origin.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
				require.NoError(t, err)
				require.True(t, durable)
				if direction == "push" {
					syncRelayMatrixNode(t, relay)
				} else {
					runner := federation.Runner{DB: relay.store, Credentials: relay.credentials, OnSyncComplete: worker.RetryNow}
					if direction == "pull_lost_ack" {
						loseAck.Store(true)
						require.Error(t, runner.RunOnce(ctx))
					} else {
						require.NoError(t, runner.RunOnce(ctx))
					}
				}
				retained, err := receiver.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
				require.NoError(t, err)
				require.Equal(t, artifact, retained, "wake follows complete durable artifact retention")
				//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
				require.Eventually(t, func() bool { return worker.Health().Backlog == 0 }, 5*time.Second, 10*time.Millisecond, "artifact-only durable arrival must wake local reconciliation")
				require.Zero(t, calls.Load(), "received compatible chunks must avoid document calls")
				if direction == "pull_lost_ack" {
					loseAck.Store(false)
					syncRelayMatrixNode(t, relay)
					require.Zero(t, calls.Load(), "lost acknowledgement retries retain exact bytes without generation")
				}
				hits, err := ix.Query(db.WithAuthorizedProjects(ctx, []string{project.UID}), client.Generation().Fingerprint(), kitvec.Vector{0, 1}, 1)
				require.NoError(t, err)
				require.Len(t, hits, 1)
				require.Equal(t, issue.UID, hits[0].Doc)
				require.Equal(t, 1, hits[0].ChunkIndex)
			})
		}
	}
}

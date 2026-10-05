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
)

// R7: reuse the existing artifact sync once per fill, bound an unavailable
// upstream to one attempt, and retain fresh authority checks for later documents.
func TestEmbeddingProducerFillConnectionBounds(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"online", "unreachable", "revoked_between_documents"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member", true)
				var metadata, offers atomic.Int32
				var reachable atomic.Bool
				reachable.Store(true)
				original := root.http.Config.Handler
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/federation/metadata") {
						metadata.Add(1)
					}
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/relay") && r.URL.Query().Get("stream") == db.RelayStreamArtifact {
						offers.Add(1)
					}
					if !reachable.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					original.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				root.http = proxy
				relay := newRelayMatrixNode(t, backend, "relay-member", true)
				project, err := root.store.CreateProject(ctx, "bounded-producer-fill")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				var inputs atomic.Int64
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Input []string `json:"input"`
					}
					require.NoError(t, json.UnmarshalRead(r.Body, &request))
					inputs.Add(int64(len(request.Input)))
					data := make([]map[string]any, len(request.Input))
					for i := range data {
						data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
					}
					require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
				}))
				t.Cleanup(provider.Close)
				client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
				require.NoError(t, err)
				recipe, err := client.ArtifactIdentity("", "", "")
				require.NoError(t, err)
				raw, err := json.Marshal(db.ProjectEmbeddingProducer{ProducerInstanceUID: relay.store.InstanceUID(), Recipe: recipe})
				require.NoError(t, err)
				_, err = root.store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
				require.NoError(t, err)
				enrollRelayMatrixReplica(t, root, relay, "bounded-relay", true)
				for _, title := range []string{"First pending document", "Second pending document", "Third pending document"} {
					_, _, err = root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "assistant"})
					require.NoError(t, err)
				}
				syncRelayMatrixNode(t, relay)
				metadata.Store(0)
				offers.Store(0)
				reachable.Store(mode != "unreachable")
				var ix *vector.Index
				if pg, ok := relay.store.(*pgstore.Store); ok {
					ix, err = vector.OpenPostgres(ctx, pg.DB)
				} else {
					ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				}
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, ix.Close()) })
				var checked atomic.Bool
				worker := daemon.NewReconciler(relay.store, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour, FederationProducerAllowed: func(ctx context.Context, uid string, recipe embedding.ArtifactIdentity, _ activity.Admission, syncArtifacts bool) (bool, error) {
					var allowed bool
					var err error
					if syncArtifacts {
						allowed, err = federation.EmbeddingProducerConnected(ctx, relay.store, relay.credentials, uid, recipe)
					} else {
						allowed, err = federation.EmbeddingProducerAuthorized(ctx, relay.store, relay.credentials, uid, recipe)
					}
					if mode == "revoked_between_documents" && allowed && !checked.Swap(true) {
						parent, e := root.store.ResolveAPIToken(ctx, root.userToken)
						require.NoError(t, e)
						_, _, e = root.store.RevokeAPIToken(ctx, parent.ID, "owner")
						require.NoError(t, e)
					}
					return allowed, err
				}})
				runCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- worker.Run(runCtx) }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(20 * time.Second):
						t.Error("worker did not stop")
					}
				})
				//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
				require.Eventually(t, func() bool { return worker.Health().LastSuccessAt != nil }, 30*time.Second, 10*time.Millisecond)
				switch mode {
				case "online":
					require.EqualValues(t, 3, inputs.Load())
					require.Zero(t, worker.Health().Backlog)
					require.EqualValues(t, 1, offers.Load(), "full artifact reconciliation is bounded per fill")
				case "unreachable":
					require.Zero(t, inputs.Load())
					require.EqualValues(t, 3, worker.Health().Backlog)
					require.EqualValues(t, 1, metadata.Load(), "one failed connection attempt defers this project's remaining fill")
					reachable.Store(true)
					worker.RetryNow()
					//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
					require.Eventually(t, func() bool { return worker.Health().Backlog == 0 }, 30*time.Second, 10*time.Millisecond)
					require.EqualValues(t, 3, inputs.Load(), "the next existing-worker fill retries the connection")
				case "revoked_between_documents":
					require.Zero(t, inputs.Load(), "revocation observed during page preparation prevents all later paid dispatch")
					require.EqualValues(t, 3, worker.Health().Backlog)
				}
			})
		}
	}
}

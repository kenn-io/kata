package federation_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// The pending-offer wait belongs to one episode of pending offers. When sync
// drains the offers and reuse fills every document, eligibility never runs,
// so the wait must end with the drain; a later episode on the same binding
// starts a new wait instead of paying at once.
func TestPendingOfferWaitRestartsAfterSyncDrains(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member", true)
			relay := newRelayMatrixNode(t, backend, "relay-member", true)
			project, err := root.store.CreateProject(ctx, "two-episode-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			var requests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var request struct {
					Input []string `json:"input"`
				}
				require.NoError(t, json.UnmarshalRead(r.Body, &request))
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
			enrollRelayMatrixReplica(t, root, relay, "two-episode-alias", true)
			retainAtRoot := func(issue db.Issue) {
				identity, err := client.ArtifactIdentity(project.UID, issue.UID, root.store.InstanceUID())
				require.NoError(t, err)
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
				require.NoError(t, err)
				durable, err := root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
				require.NoError(t, err)
				require.True(t, durable)
			}
			var ix *vector.Index
			if pg, ok := relay.store.(*pgstore.Store); ok {
				ix, err = vector.OpenPostgres(ctx, pg.DB)
			} else {
				ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, ix.Close()) })
			runWorker := func() *daemon.Reconciler {
				worker := daemon.NewReconciler(relay.store, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour, FederationProducerAllowed: func(ctx context.Context, uid string, recipe embedding.ArtifactIdentity, _ activity.Admission, syncArtifacts bool) (bool, error) {
					if !syncArtifacts {
						return federation.EmbeddingProducerAuthorized(ctx, relay.store, relay.credentials, uid, recipe)
					}
					return federation.EmbeddingProducerConnected(ctx, relay.store, relay.credentials, uid, recipe)
				}})
				runCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { done <- worker.Run(runCtx) }()
				//nolint:kennlint // sleeptest: Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
				require.Eventually(t, func() bool { return worker.Health().LastSuccessAt != nil }, 20*time.Second, 10*time.Millisecond)
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Fatal("worker did not stop")
				}
				return worker
			}

			first, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "First episode", Author: "assistant"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, relay)
			retainAtRoot(first)
			worker := runWorker()
			require.Zero(t, requests.Load())
			require.EqualValues(t, 1, worker.Health().Backlog, "the first episode waits for the pending offer")
			require.Contains(t, worker.Health().ProducerErrors[relay.project.UID].Message, "waiting for federation sync")

			syncRelayMatrixNode(t, relay)
			worker = runWorker()
			require.Zero(t, requests.Load(), "the synced artifact is reused")
			require.Zero(t, worker.Health().Backlog, "reuse fills the only pending document without consulting eligibility")

			t.Cleanup(federation.SetEmbeddingProducerClockForTest(func() time.Time {
				return time.Now().Add(federation.EmbeddingProducerPendingArtifactWait + time.Second)
			}))
			second, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: "Second episode", Author: "assistant"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, relay)
			retainAtRoot(second)
			worker = runWorker()
			require.Zero(t, requests.Load(), "a new episode of pending offers waits again instead of paying at once")
			require.EqualValues(t, 1, worker.Health().Backlog)
			require.Contains(t, worker.Health().ProducerErrors[relay.project.UID].Message, "waiting for federation sync")
		})
	}
}

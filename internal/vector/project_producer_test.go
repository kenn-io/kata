package vector_test

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

	"go.kenn.io/kata/internal/activity"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

// Revised R7: an unavailable designated producer does not cause another node
// to generate shared vectors. Pending content and the stable assignment survive.
func TestEmbeddingProjectWaitsForDesignatedProducer(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"wait", "local_producer", "private", "wait_then_private", "invalid_then_private", "paused_local", "paused_legacy", "root_default"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				var source db.Storage
				var ix *vector.Index
				if backend == "sqlite" {
					s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
					require.NoError(t, err)
					source = s
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					ix, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, ix.Close()) })
				} else {
					dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
					t.Cleanup(cleanup)
					s, err := pgstore.Open(ctx, dsn)
					require.NoError(t, err)
					source = s
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					ix, err = vector.OpenPostgres(ctx, s.DB)
					require.NoError(t, err)
				}
				var requests, privateRequests atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Input []string `json:"input"`
					}
					require.NoError(t, json.UnmarshalRead(r.Body, &request))
					if len(request.Input) == 1 && request.Input[0] == "Private local content\n\n" {
						privateRequests.Add(1)
					} else {
						requests.Add(1)
					}
					data := []map[string]any{}
					for i := range request.Input {
						data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0}})
					}
					require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
				}))
				t.Cleanup(provider.Close)
				client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
				require.NoError(t, err)
				project, err := source.CreateProject(ctx, "producer-wait-project")
				require.NoError(t, err)
				if mode != "private" {
					_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: mode != "paused_local" && mode != "paused_legacy"})
					require.NoError(t, err)
				}
				producerUID := "00000000000000000000000002"
				if mode == "local_producer" || mode == "paused_local" || mode == "root_default" {
					producerUID = source.InstanceUID()
				}
				recipe, err := client.ArtifactIdentity("", "", "")
				require.NoError(t, err)
				configuration, err := json.Marshal(map[string]any{"producer_instance_uid": producerUID, "recipe": recipe})
				require.NoError(t, err)
				if mode == "invalid_then_private" {
					var invalid db.ProjectEmbeddingProducer
					require.NoError(t, json.Unmarshal(configuration, &invalid))
					invalid.Recipe.Dimensions = 0
					configuration, err = json.Marshal(invalid)
					require.NoError(t, err)
					blob, err := json.Marshal(map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: configuration})
					require.NoError(t, err)
					// A retained owner backup can contain an opaque value from before validation.
					if pg, ok := source.(*pgstore.Store); ok {
						_, err = pg.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(blob), project.ID)
					} else {
						_, err = source.(*sqlitestore.Store).ExecContext(ctx, `UPDATE projects SET metadata=? WHERE id=?`, string(blob), project.ID)
					}
					require.NoError(t, err)
				} else if mode == "root_default" {
					public, _, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					require.NoError(t, source.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: source.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				} else if mode != "paused_legacy" {
					_, err = source.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "member", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: configuration}})
					require.NoError(t, err)
				}
				_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Keep pending content", Body: strings.Repeat("界", 2100), Author: "member"})
				require.NoError(t, err)
				if mode == "wait_then_private" || mode == "invalid_then_private" {
					privateProject, err := source.CreateProject(ctx, "private-generation-project")
					require.NoError(t, err)
					_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: privateProject.ID, Title: "Private local content", Author: "member"})
					require.NoError(t, err)
				}
				broadcast := daemon.NewEventBroadcaster()
				publisher := daemon.NewEventPublisher(broadcast, nil)
				sub := broadcast.Subscribe(daemon.SubFilter{ProjectID: project.ID})
				t.Cleanup(sub.Unsub)
				reconciler := daemon.NewReconciler(source, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour, BatchSize: 1, OnProjectEvent: func(event db.Event, fork activity.Admission) { publisher.EventFrom(event.ProjectID, event, fork) }})
				runCtx, cancel := context.WithCancel(ctx)
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() { done <- reconciler.Run(runCtx) }()
				//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
				require.Eventually(t, func() bool { return reconciler.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("embedding worker did not stop")
				}
				if mode == "local_producer" || mode == "private" || mode == "paused_local" || mode == "paused_legacy" || mode == "root_default" {
					require.EqualValues(t, 1, requests.Load(), "successful generation uses the existing local worker baseline")
					require.Zero(t, reconciler.Health().Backlog)
					require.EqualValues(t, 1, reconciler.Health().Embedded)
				} else {
					require.Zero(t, requests.Load(), "another designated producer is unavailable; do not fall back to paid generation")
					require.EqualValues(t, 1, reconciler.Health().Backlog)
					if mode == "wait_then_private" || mode == "invalid_then_private" {
						require.EqualValues(t, 1, privateRequests.Load(), "waiting shared content cannot starve later private generation")
						require.EqualValues(t, 1, reconciler.Health().Embedded)
					} else {
						require.Zero(t, reconciler.Health().Embedded)
					}
				}
				retained, err := source.ProjectByID(ctx, project.ID)
				require.NoError(t, err)
				var metadata map[string]jsontext.Value
				require.NoError(t, json.Unmarshal([]byte(retained.Metadata), &metadata))
				if mode != "paused_legacy" {
					require.JSONEq(t, string(configuration), string(metadata["federation_embedding"]))
				} else {
					require.NotContains(t, metadata, db.ProjectEmbeddingMetadataKey)
				}
				if mode == "root_default" {
					select {
					case msg := <-sub.Ch:
						require.NotNil(t, msg.Event)
						require.Equal(t, "project.metadata_updated", msg.Event.Type)
						require.Equal(t, "system", msg.Event.Actor)
						events, err := source.EventsByUIDs(ctx, project.ID, []string{msg.Event.UID})
						require.NoError(t, err)
						require.Len(t, events, 1)
					case <-time.After(time.Second):
						t.Fatal("producer assignment was not published")
					}
				}
				if mode == "local_producer" || mode == "paused_local" || mode == "root_default" {
					_, err := source.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "member", Patch: map[string]jsontext.Value{"area": jsontext.Value(`"example-area"`)}})
					require.NoError(t, err)
					replay := daemon.NewReconciler(source, ix, client, daemon.ReconcilerConfig{SweepEvery: time.Hour})
					replayCtx, stop := context.WithCancel(ctx)
					t.Cleanup(stop)
					replayed := make(chan error, 1)
					go func() { replayed <- replay.Run(replayCtx) }()
					//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
					require.Eventually(t, func() bool { return replay.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
					stop()
					select {
					case <-replayed:
					case <-time.After(10 * time.Second):
						t.Fatal("restarted embedding worker did not stop")
					}
					require.EqualValues(t, 1, requests.Load(), "metadata changes and restart after artifact persistence make no additional provider requests")
				}
			})
		}
	}
}

package vector_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// R7: an upstream eligibility check can change canonical content after the
// mirror scan. Stale input must be deferred before paid dispatch, and the next
// normal mirror refresh must reuse the received current-input artifact.
func TestEmbeddingCanonicalChangeBeforeDispatch(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
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
				release, err := ix.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}
			project, err := source.CreateProject(ctx, "dispatch-reuse-project")
			require.NoError(t, err)
			issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Complete received chunks", Body: strings.Repeat("界", 2500), Author: "member"})
			require.NoError(t, err)
			_, err = ix.RefreshMirror(ctx, source)
			require.NoError(t, err)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var request struct {
					Input []string `json:"input"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				data := []map[string]any{}
				for i := range request.Input {
					data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0}})
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
			}))
			t.Cleanup(server.Close)
			client, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "example-model", Dims: 2})
			require.NoError(t, err)
			recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
			require.NoError(t, err)
			key := client.Generation().Fingerprint()
			require.NoError(t, ix.EnsureBuilding(ctx, key, client.Generation()))
			n, err := ix.ReconcileArtifacts(ctx, key, source, recipe)
			require.NoError(t, err)
			require.Zero(t, n, "no artifact exists during the initial reconciliation")
			identity := recipe
			identity.ProjectUID, identity.IssueUID = project.UID, issue.UID
			identity.ProducerInstanceUID = "00000000000000000000000002"

			changedTitle := "Current input from upstream"
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(changedTitle, issue.Body), [][]float32{{1, 0}, {0, 1}})
			require.NoError(t, err)
			arrived := false
			allowed := func(ctx context.Context, projectUID string) (bool, error) {
				require.Equal(t, project.UID, projectUID)
				if !arrived {
					arrived = true
					_, _, _, err := source.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &changedTitle, Actor: "member"})
					require.NoError(t, err)
					durable, err := source.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
					require.NoError(t, err)
					require.True(t, durable)
				}
				return true, nil
			}
			_, err = ix.FillWithArtifacts(ctx, key, source, recipe, client.EncodeFunc(), 1, nil, nil, allowed)
			require.NoError(t, err)
			require.True(t, arrived)
			require.Zero(t, requests.Load(), "canonical input changed during eligibility; do not pay for the stale scan")
			_, err = ix.RefreshMirror(ctx, source)
			require.NoError(t, err)
			_, err = ix.FillWithArtifacts(ctx, key, source, recipe, client.EncodeFunc(), 1, nil, nil, allowed)
			require.NoError(t, err)
			require.Zero(t, requests.Load(), "current received vectors are reused after the normal mirror refresh")
			backlog, err := ix.Backlog(ctx, key)
			require.NoError(t, err)
			require.Zero(t, backlog)
			hits, err := ix.Query(ctx, key, kitvec.Vector{0, 1}, 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, issue.UID, hits[0].Doc)
			require.Equal(t, 1, hits[0].ChunkIndex)
			reused, err := ix.ReconcileArtifacts(ctx, key, source, recipe)
			require.NoError(t, err)
			require.Zero(t, reused, "an unchanged sweep must not rewrite current index chunks")
		})
	}
}

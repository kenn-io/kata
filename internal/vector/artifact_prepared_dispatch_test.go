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

// R3/R7/A13: preparing another document may observe revocation or receive
// vectors after the first document was prepared. Revalidate at paid dispatch.
func TestEmbeddingPreparedPageRechecksBeforePaidDispatch(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"revoke", "received", "revoke_parallel", "received_parallel"} {
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
					release, err := ix.AcquireReconcilerLease(ctx)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, release()) })
				}

				project, err := source.CreateProject(ctx, "prepared-page-project")
				require.NoError(t, err)
				var issues []db.Issue
				for _, title := range []string{"First prepared document", "Second prepared document"} {
					issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Body: strings.Repeat("界", 2500), Author: "member"})
					require.NoError(t, err)
					issues = append(issues, issue)
				}
				_, err = ix.RefreshMirror(ctx, source)
				require.NoError(t, err)
				var requests atomic.Int32
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var input struct {
						Input []string `json:"input"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
					data := []map[string]any{}
					for i := range input.Input {
						data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0}})
					}
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
				}))
				t.Cleanup(provider.Close)
				client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
				require.NoError(t, err)
				recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
				require.NoError(t, err)
				key := client.Generation().Fingerprint()
				require.NoError(t, ix.EnsureBuilding(ctx, key, client.Generation()))
				checks, revoked := 0, false
				var batchOptions []kitvec.BatchOption
				expectedRequests := 1
				if strings.HasSuffix(mode, "_parallel") {
					batchOptions = []kitvec.BatchOption{kitvec.WithBatchSize(1), kitvec.WithBatchConcurrency(4)}
					expectedRequests = 4
				}
				allowed := func(ctx context.Context, projectUID string) (bool, error) {
					require.Equal(t, project.UID, projectUID)
					checks++
					if checks == 2 {
						if strings.HasPrefix(mode, "revoke") {
							revoked = true
						} else {
							for _, issue := range issues {
								identity := recipe
								identity.ProjectUID, identity.IssueUID = project.UID, issue.UID
								artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}, {1, 0}})
								require.NoError(t, err)
								retained, err := source.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
								require.NoError(t, err)
								require.True(t, retained)
							}
						}
					}
					return !revoked, nil
				}
				_, err = ix.FillWithArtifacts(ctx, key, source, recipe, client.EncodeFunc(), 2, batchOptions, nil, allowed)
				if err != nil {
					require.ErrorIs(t, err, kitvec.ErrStale)
				}
				require.GreaterOrEqual(t, checks, 2)
				require.Zero(t, requests.Load(), "a prepared page must not dispatch known-revoked or already-received input")
				backlog, err := ix.Backlog(ctx, key)
				require.NoError(t, err)
				if strings.HasPrefix(mode, "revoke") {
					require.EqualValues(t, 2, backlog)
				}
				_, err = ix.FillWithArtifacts(ctx, key, source, recipe, client.EncodeFunc(), 2, batchOptions, nil, func(context.Context, string) (bool, error) { return true, nil })
				require.NoError(t, err)
				if strings.HasPrefix(mode, "received") {
					require.Zero(t, requests.Load())
				} else {
					require.EqualValues(t, expectedRequests, requests.Load(), "reconnection keeps the one-worker successful request baseline")
				}
				backlog, err = ix.Backlog(ctx, key)
				require.NoError(t, err)
				require.Zero(t, backlog)
			})
		}
	}
}

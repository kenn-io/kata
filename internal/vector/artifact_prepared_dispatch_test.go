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

// R3/R7/A13: preparing another document may observe revocation, receive
// vectors or see an edit after the first document was prepared. Revalidate at
// paid dispatch. A stale document stays pending without aborting the fill, and
// the page's other documents still proceed.
func TestEmbeddingPreparedPageRechecksBeforePaidDispatch(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"revoke", "received", "edited", "revoke_parallel", "received_parallel", "edited_parallel"} {
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
				expectedRequests, editedRequests := 1, 1
				if strings.HasSuffix(mode, "_parallel") {
					batchOptions = []kitvec.BatchOption{kitvec.WithBatchSize(1), kitvec.WithBatchConcurrency(4)}
					expectedRequests, editedRequests = 4, 2
				}
				allowed := func(ctx context.Context, projectUID string) (bool, error) {
					require.Equal(t, project.UID, projectUID)
					checks++
					if checks == 2 {
						switch {
						case strings.HasPrefix(mode, "revoke"):
							revoked = true
						case strings.HasPrefix(mode, "edited"):
							edited := "Edited prepared document"
							_, _, _, err := source.EditIssue(ctx, db.EditIssueParams{IssueID: issues[0].ID, Title: &edited, Actor: "member"})
							require.NoError(t, err)
						default:
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
				require.NoError(t, err, "a deferred document must not abort the fill")
				require.GreaterOrEqual(t, checks, 2)
				backlog, err := ix.Backlog(ctx, key)
				require.NoError(t, err)
				switch {
				case strings.HasPrefix(mode, "revoke"):
					require.Zero(t, requests.Load(), "a prepared page must not dispatch known-revoked input")
					require.EqualValues(t, 2, backlog)
				case strings.HasPrefix(mode, "edited"):
					require.EqualValues(t, editedRequests, requests.Load(), "only the unchanged document is dispatched")
					require.EqualValues(t, 1, backlog, "the edited document stays pending")
					_, err = ix.RefreshMirror(ctx, source)
					require.NoError(t, err)
					requests.Store(0)
				default:
					require.Zero(t, requests.Load(), "a prepared page must not dispatch already-received input")
					require.Zero(t, backlog)
				}
				_, err = ix.FillWithArtifacts(ctx, key, source, recipe, client.EncodeFunc(), 2, batchOptions, nil, func(context.Context, string) (bool, error) { return true, nil })
				require.NoError(t, err)
				switch {
				case strings.HasPrefix(mode, "received"):
					require.Zero(t, requests.Load())
				case strings.HasPrefix(mode, "edited"):
					require.EqualValues(t, editedRequests, requests.Load(), "the next fill embeds only the edited document")
				default:
					require.EqualValues(t, expectedRequests, requests.Load(), "reconnection keeps the one-worker successful request baseline")
				}
				backlog, err = ix.Backlog(ctx, key)
				require.NoError(t, err)
				require.Zero(t, backlog)
			})
		}
	}
}

func TestEmbeddingPreparedPageRevalidatesProjectAuthorityOncePerDispatchBatch(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"serial", "parallel"} {
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

				project, err := source.CreateProject(ctx, "authority-batch-project")
				require.NoError(t, err)
				for _, title := range []string{"First authority document", "Second authority document"} {
					_, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
						ProjectID: project.ID, Title: title, Body: strings.Repeat("界", 2500), Author: "member",
					})
					require.NoError(t, err)
				}
				_, err = ix.RefreshMirror(ctx, source)
				require.NoError(t, err)
				client, err := embedding.New(embedding.Config{BaseURL: "https://encoder.example/v1", Model: "example-model", Dims: 2})
				require.NoError(t, err)
				recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
				require.NoError(t, err)
				key := client.Generation().Fingerprint()
				require.NoError(t, ix.EnsureBuilding(ctx, key, client.Generation()))

				options := []kitvec.BatchOption(nil)
				expectedDispatches := int32(1)
				if mode == "parallel" {
					options = []kitvec.BatchOption{kitvec.WithBatchSize(1), kitvec.WithBatchConcurrency(4)}
					expectedDispatches = 4
				}
				var checks, dispatches atomic.Int32
				eligible := func(_ context.Context, projectUID string) (bool, error) {
					require.Equal(t, project.UID, projectUID)
					checks.Add(1)
					return true, nil
				}
				encode := func(_ context.Context, texts []string) ([][]float32, error) {
					dispatches.Add(1)
					vectors := make([][]float32, len(texts))
					for i := range vectors {
						vectors[i] = []float32{1, 0}
					}
					return vectors, nil
				}

				_, err = ix.FillWithArtifacts(ctx, key, source, recipe, encode, 2, options, nil, eligible)
				require.NoError(t, err)
				require.Equal(t, expectedDispatches, dispatches.Load())
				// Preparing the page authorizes the shared project once for both
				// documents. Each actual encoder batch then revalidates it exactly once.
				require.Equal(t, int32(1)+expectedDispatches, checks.Load())
			})
		}
	}
}

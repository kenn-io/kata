package vector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// The project boundary belongs before distance ranking and its candidate limit:
// arbitrarily closer private chunks must never crowd out an authorized result.
func TestProjectAccessVectorCandidatesBeforeLimit(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var store db.Storage
			var idx *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, idx.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
				release, err := idx.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}
			allowed, err := store.CreateProject(ctx, "allowed-project")
			require.NoError(t, err)
			private, err := store.CreateProject(ctx, "private-project")
			require.NoError(t, err)
			target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: allowed.ID, Title: "allowed-result", Author: "member"})
			require.NoError(t, err)
			for i := range 12 {
				_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: private.ID, Title: fmt.Sprintf("private-result-%d", i), Author: "member"})
				require.NoError(t, err)
			}
			_, err = idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			var queryCalls atomic.Int32
			embedder := mappedEmbedder(t, func(string) []float32 { queryCalls.Add(1); return []float32{1, 0} })
			gen := embedder.Generation()
			key := gen.Fingerprint()
			require.NoError(t, idx.EnsureBuilding(ctx, key, gen))
			encoded := 0
			_, err = idx.Fill(ctx, key, func(_ context.Context, texts []string) ([][]float32, error) {
				encoded += len(texts)
				vectors := make([][]float32, len(texts))
				for i, text := range texts {
					if strings.Contains(text, "allowed-result") {
						vectors[i] = []float32{0.8, 0.6}
					} else {
						vectors[i] = []float32{1, 0}
					}
				}
				return vectors, nil
			}, 0, nil, nil)
			require.NoError(t, err)
			require.Equal(t, 13, encoded)
			scoped := db.WithAuthorizedProjects(ctx, []string{allowed.UID})
			window, err := idx.QueryWithProbe(scoped, key, kitvec.Vector{1, 0}, 1)
			require.NoError(t, err)
			require.Len(t, window.Hits, 1)
			require.Equal(t, target.UID, window.Hits[0].Doc)
			require.False(t, window.HasProbe, "private chunks do not count as an eligible probe")
			hits, err := idx.Query(scoped, key, kitvec.Vector{1, 0}, 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, target.UID, hits[0].Doc)
			hits, err = idx.Query(db.WithAuthorizedProjects(ctx, []string{}), key, kitvec.Vector{1, 0}, 1)
			require.NoError(t, err)
			require.Empty(t, hits)
			require.Equal(t, 13, encoded, "retrieval uses existing vectors")
			require.NoError(t, idx.CutOver(ctx, key))
			team, _, err := store.CreateTeam(ctx, "search-team", "admin")
			require.NoError(t, err)
			_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: private.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
			require.NoError(t, err)
			_, _, err = store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "search-test-token", Actor: "member", AdminActor: "admin"})
			require.NoError(t, err)
			server := daemon.NewServer(daemon.ServerConfig{DB: store, Embedder: embedder, VectorIndex: idx, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			for _, test := range []struct {
				project db.Project
				status  int
			}{{allowed, http.StatusOK}, {private, http.StatusNotFound}} {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/search?q=semantic-probe&mode=semantic", httpServer.URL, test.project.ID), nil)
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer search-test-token")
				response, err := httpServer.Client().Do(request)
				require.NoError(t, err)
				raw, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, test.status, response.StatusCode, string(raw))
				require.NotContains(t, string(raw), "private-result-")
				if test.status == http.StatusOK {
					require.Contains(t, string(raw), target.UID)
				}
			}
			require.EqualValues(t, 1, queryCalls.Load(), "only the authorized search embeds its query")

		})
	}
}

// R7/A12: importing all received chunks answers real KNN without document
// encoding, rejects a stale local revision, and enforces the recipient recipe.
func TestEmbeddingArtifactImportSemanticRetrieval(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var store db.Storage
			var idx *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, idx.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
				release, err := idx.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}
			importer, ok := any(idx).(interface {
				ImportArtifact(context.Context, string, embedding.EmbeddingArtifact, embedding.ArtifactIdentity, any) error
			})
			require.True(t, ok, "index must import exact complete portable artifacts under its existing lease")
			project, err := store.CreateProject(ctx, "received-vector-project")
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Received complete vectors", Body: strings.Repeat("界", 2500), Author: "member"})
			require.NoError(t, err)
			_, err = idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			content, err := store.ListIssueContent(ctx, 0, 10)
			require.NoError(t, err)
			require.Len(t, content, 1)
			identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: "00000000000000000000000002", Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "l2", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}, {0, 1}})
			require.NoError(t, err)
			require.Len(t, artifact.Chunks, 2)
			generation := kitvec.Generation{Model: "example-model", Dimensions: 2, Params: map[string]string{"recipe": "2"}}
			key := generation.Fingerprint()
			require.NoError(t, idx.EnsureBuilding(ctx, key, generation))
			expected := artifact.ArtifactIdentity
			expected.ProducerInstanceUID = store.InstanceUID()
			require.ErrorIs(t, importer.ImportArtifact(ctx, key, artifact, expected, content[0].ContentRevision+1), kitvec.ErrStale)
			require.ErrorIs(t, importer.ImportArtifact(db.WithAuthorizedProjects(ctx, []string{}), key, artifact, expected, content[0].ContentRevision), db.ErrNotFound)
			for _, mode := range []string{"recipe", "model", "revision", "input_type", "normalization", "dimensions", "project"} {
				wrong := expected
				switch mode {
				case "recipe":
					wrong.RecipeFingerprint = strings.Repeat("b", 64)
				case "model":
					wrong.Model = "other-model"
				case "revision":
					wrong.ModelRevision = "other-revision"
				case "input_type":
					wrong.InputType = "retrieval"
				case "normalization":
					wrong.Normalization = "none"
				case "dimensions":
					wrong.Dimensions = 3
				case "project":
					wrong.ProjectUID = "00000000000000000000000005"
				}
				require.Error(t, importer.ImportArtifact(ctx, key, artifact, wrong, content[0].ContentRevision), mode)
			}
			pending, err := idx.Backlog(ctx, key)
			require.NoError(t, err)
			require.EqualValues(t, 1, pending, "rejected imports never stamp content")
			require.NoError(t, importer.ImportArtifact(ctx, key, artifact, expected, content[0].ContentRevision))
			require.NoError(t, importer.ImportArtifact(ctx, key, artifact, expected, content[0].ContentRevision), "exact replay is idempotent")
			require.NoError(t, idx.CutOver(ctx, key))
			var calls atomic.Int32
			_, err = idx.Fill(ctx, key, func(_ context.Context, _ []string) ([][]float32, error) {
				calls.Add(1)
				return nil, fmt.Errorf("unexpected document provider call")
			}, 0, nil, nil)
			require.NoError(t, err)
			require.Zero(t, calls.Load(), "complete received vectors leave no paid document fill")
			hits, err := idx.Query(db.WithAuthorizedProjects(ctx, []string{project.UID}), key, kitvec.Vector{0, 1}, 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, issue.UID, hits[0].Doc)
			require.Equal(t, 1, hits[0].ChunkIndex, "retrieval must use the second chunk, not a first-vector or averaged export")
		})
	}
}

// R7/A12: a complete compatible received artifact is reconciled before paid
// fill, and its second chunk answers an actual query from the same encoder.
func TestEmbeddingArtifactReuseBeforeFill(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var documentCalls, queryCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input []string `json:"input"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				data := []map[string]any{}
				for i, input := range request.Input {
					vector := []float32{1, 0}
					if input == "query for received second chunk" {
						queryCalls.Add(1)
						vector = []float32{0, 1}
					} else {
						documentCalls.Add(1)
					}
					data = append(data, map[string]any{"index": i, "embedding": vector})
				}
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
			}))
			t.Cleanup(server.Close)
			embedder, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "example-model", Dims: 2})
			require.NoError(t, err)
			var store db.Storage
			var idx *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, idx.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
			}
			project, err := store.CreateProject(ctx, "artifact-reuse-project")
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Received complete vectors", Body: strings.Repeat("界", 2500), Author: "source-assistant"})
			require.NoError(t, err)
			identity, err := embedder.ArtifactIdentity(project.UID, issue.UID, "00000000000000000000000002")
			require.NoError(t, err)
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}, {0, 1}})
			require.NoError(t, err)
			durable, err := store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
			require.NoError(t, err)
			require.True(t, durable)
			runCtx, cancel := context.WithCancel(ctx)
			reconciler := daemon.NewReconciler(store, idx, embedder, daemon.ReconcilerConfig{SweepEvery: time.Hour})
			done := make(chan error, 1)
			go func() { done <- reconciler.Run(runCtx) }()
			t.Cleanup(cancel)
			//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
			require.Eventually(t, func() bool {
				health := reconciler.Health()
				return health.LastSuccessAt != nil && health.Backlog == 0 && health.Embedded == 1
			}, 10*time.Second, 10*time.Millisecond, "received artifact must finish semantic backfill")
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				require.FailNow(t, "reconciler did not stop")
			}
			require.Zero(t, documentCalls.Load(), "received compatible artifact must prevent paid document calls")
			queries, err := embedder.Embed(ctx, []string{"query for received second chunk"})
			require.NoError(t, err)
			hits, err := idx.Query(db.WithAuthorizedProjects(ctx, []string{project.UID}), embedder.Generation().Fingerprint(), kitvec.Vector(queries[0]), 1)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, issue.UID, hits[0].Doc)
			require.Equal(t, 1, hits[0].ChunkIndex)
			require.EqualValues(t, 1, queryCalls.Load())
		})
	}
}

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
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

// R7/A12: the existing worker retains every original float32 chunk before
// backend conversion, using the scanned input even if it changes during a call.
func TestEmbeddingArtifactCaptureBeforeBackendConversion(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"stable", "edit_during_call"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
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
				}
				project, err := store.CreateProject(ctx, "artifact-capture-project")
				require.NoError(t, err)
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Original embedding scan", Body: strings.Repeat("界", 2500), Author: "member"})
				require.NoError(t, err)
				originalInput := embedding.EmbedText(issue.Title, issue.Body)
				var requests, inputs atomic.Int64
				var edited atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var request struct {
						Input []string `json:"input"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					inputs.Add(int64(len(request.Input)))
					if mode == "edit_during_call" && edited.CompareAndSwap(false, true) {
						title := "Edited after embedding scan"
						_, _, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "member"})
						require.NoError(t, err)
						_, err = idx.RefreshMirror(ctx, store)
						require.NoError(t, err)
					}
					data := []map[string]any{}
					for i := range request.Input {
						data = append(data, map[string]any{"index": i, "embedding": []float32{0.6, 0.8}})
					}
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
				}))
				t.Cleanup(server.Close)
				embedder, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "example-model", Dims: 2})
				require.NoError(t, err)
				runCtx, cancel := context.WithCancel(ctx)
				reconciler := daemon.NewReconciler(store, idx, embedder, daemon.ReconcilerConfig{SweepEvery: time.Hour})
				done := make(chan error, 1)
				go func() { done <- reconciler.Run(runCtx) }()
				t.Cleanup(cancel)
				//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
				require.Eventually(t, func() bool { return reconciler.Health().LastSuccessAt != nil }, 10*time.Second, 10*time.Millisecond)
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					require.FailNow(t, "reconciler did not stop")
				}
				require.EqualValues(t, 1, requests.Load(), "successful baseline is one local worker request")
				require.EqualValues(t, 2, inputs.Load(), "the complete scanned recipe has two chunks")
				manifests, err := store.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, project.UID, 10)
				require.NoError(t, err)
				require.Len(t, manifests, 1, "successful document vectors must enter canonical portable storage")
				artifact, err := store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, manifests[0].Digest)
				require.NoError(t, err)
				identity, err := embedder.ArtifactIdentity(project.UID, issue.UID, store.InstanceUID())
				require.NoError(t, err)
				require.NoError(t, embedding.ValidateArtifact(artifact, originalInput, identity.RecipeFingerprint))
				require.Equal(t, embedding.ArtifactInputHash(originalInput), artifact.InputHash, "retain the actual paid input, not a post-call edit")
				require.Equal(t, store.InstanceUID(), artifact.ProducerInstanceUID)
				require.Len(t, artifact.Chunks, 2)
				for _, chunk := range artifact.Chunks {
					// IEEE float32 0.6 and 0.8, little endian; PG halfvec cannot retain these bits.
					require.Equal(t, []byte{0x9a, 0x99, 0x19, 0x3f, 0xcd, 0xcc, 0x4c, 0x3f}, chunk.VectorBytes)
				}
				if mode == "edit_during_call" {
					require.EqualValues(t, 1, reconciler.Health().Backlog, "a concurrent edit remains pending rather than accepting old vectors")
				}
			})
		}
	}
}

// R7 distinguishes portable transport limits from local index limits. Capture
// must preserve existing local generation when a document has too many chunks
// to form a bounded portable artifact, rather than retrying successful calls.
func TestArtifactCaptureBoundsPreserveLocalIndexing(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var source db.Storage
			var idx *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, idx.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
				release, err := idx.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}
			project, err := source.CreateProject(ctx, "local-generation-project")
			require.NoError(t, err)
			_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Local large input", Body: strings.Repeat("x", 2000+1800*embedding.MaxArtifactChunks), Author: "member"})
			require.NoError(t, err)
			_, err = idx.RefreshMirror(ctx, source)
			require.NoError(t, err)
			client, err := embedding.New(embedding.Config{BaseURL: "https://encoder.example/v1", Model: "example-model", Dims: 2})
			require.NoError(t, err)
			recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
			require.NoError(t, err)
			generation := client.Generation()
			key := generation.Fingerprint()
			require.NoError(t, idx.EnsureBuilding(ctx, key, generation))
			var inputs int
			encode := func(_ context.Context, texts []string) ([][]float32, error) {
				inputs += len(texts)
				values := make([][]float32, len(texts))
				for i := range values {
					values[i] = []float32{1, 0}
				}
				return values, nil
			}
			stats, err := idx.FillWithArtifacts(ctx, key, source, recipe, encode, 0, nil, nil)
			require.NoError(t, err, "portable limits must not turn successful local generation into a retry loop")
			require.Greater(t, stats.Chunks, embedding.MaxArtifactChunks)
			require.Equal(t, stats.Chunks, inputs)
			embedded, skipped, pending, err := idx.Coverage(ctx, key)
			require.NoError(t, err)
			require.EqualValues(t, 1, embedded)
			require.Zero(t, skipped)
			require.Zero(t, pending)
			manifests, err := source.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, project.UID, 10)
			require.NoError(t, err)
			require.Empty(t, manifests, "oversize local vectors never masquerade as a complete bounded artifact")
		})
	}
}

// Portable artifact capture must preserve the existing local fill contract
// when the raw splitter window exceeds the bounded artifact format, even when
// whitespace keeps the actual encoded chunk count small.
func TestArtifactCaptureRuneWindowBoundsPreserveLocalIndexing(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var source db.Storage
			var idx *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, idx.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				source = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
				release, err := idx.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}

			project, err := source.CreateProject(ctx, "rune-window-project")
			require.NoError(t, err)
			maxRawRunes := embedding.RecipeSplitMaxRunes +
				(embedding.MaxArtifactChunks-1)*(embedding.RecipeSplitMaxRunes-embedding.RecipeSplitOverlap)
			title := "Whitespace window"
			body := strings.Repeat("prefix ", 10) + strings.Repeat(" ", maxRawRunes) + "suffix"
			require.Less(t, len(embedding.EmbedText(title, body)), embedding.MaxArtifactInputBytes,
				"input stays below the byte limit while its raw rune window exceeds the portable limit")
			_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID,
				Title:     title,
				Body:      body,
				Author:    "member",
			})
			require.NoError(t, err)
			_, err = idx.RefreshMirror(ctx, source)
			require.NoError(t, err)
			client, err := embedding.New(embedding.Config{BaseURL: "https://encoder.example/v1", Model: "example-model", Dims: 2})
			require.NoError(t, err)
			recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
			require.NoError(t, err)
			key := client.Generation().Fingerprint()
			require.NoError(t, idx.EnsureBuilding(ctx, key, client.Generation()))
			encode := func(_ context.Context, texts []string) ([][]float32, error) {
				values := make([][]float32, len(texts))
				for i := range values {
					values[i] = []float32{1, 0}
				}
				return values, nil
			}

			stats, err := idx.FillWithArtifacts(ctx, key, source, recipe, encode, 0, nil, nil)
			require.NoError(t, err, "successful local vectors must not fail portable artifact capture")
			require.Positive(t, stats.Chunks)
			embedded, skipped, pending, err := idx.Coverage(ctx, key)
			require.NoError(t, err)
			require.EqualValues(t, 1, embedded)
			require.Zero(t, skipped)
			require.Zero(t, pending)
			manifests, err := source.(db.EmbeddingArtifactStorage).EmbeddingArtifactManifests(ctx, project.UID, 10)
			require.NoError(t, err)
			require.Empty(t, manifests, "inputs beyond the portable raw window remain local-only")
		})
	}
}

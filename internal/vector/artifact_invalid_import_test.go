package vector_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

// A retained artifact can claim an input that the issue later returns to while
// carrying chunks for different text. Its import fails validation; that one
// artifact must not stop artifact reconciliation or the local fill.
func TestEmbeddingInvalidRetainedArtifactDoesNotStopFill(t *testing.T) {
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
			var requests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var input struct {
					Input []string `json:"input"`
				}
				require.NoError(t, json.UnmarshalRead(r.Body, &input))
				data := []map[string]any{}
				for i := range input.Input {
					data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0}})
				}
				require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
			}))
			t.Cleanup(provider.Close)
			client, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "example-model", Dims: 2})
			require.NoError(t, err)
			recipe, err := client.ArtifactIdentity("", "", source.InstanceUID())
			require.NoError(t, err)

			project, err := source.CreateProject(ctx, "invalid-artifact-project")
			require.NoError(t, err)
			issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Draft title", Author: "member"})
			require.NoError(t, err)
			claimed := embedding.EmbedText("Final title", "")
			identity := recipe
			identity.ProjectUID, identity.IssueUID = project.UID, issue.UID
			mismatched := mismatchedInputArtifact(t, identity, claimed, embedding.EmbedText("Other text", ""))
			retained, err := source.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, mismatched)
			require.NoError(t, err)
			require.True(t, retained, "a stale-input artifact is retained without chunk validation")
			final := "Final title"
			_, _, _, err = source.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &final, Actor: "member"})
			require.NoError(t, err)

			key := client.Generation().Fingerprint()
			require.NoError(t, ix.EnsureBuilding(ctx, key, client.Generation()))
			_, err = ix.RefreshMirror(ctx, source)
			require.NoError(t, err)
			reused, err := ix.ReconcileArtifacts(ctx, key, source, recipe)
			require.NoError(t, err, "one invalid artifact must not stop reconciliation")
			require.Zero(t, reused)
			require.True(t, ix.ArtifactRejected(mismatched.Digest), "the rejected artifact is visible to status")
			_, err = ix.FillWithArtifacts(ctx, key, source, recipe, client.EncodeFunc(), 8, nil, nil)
			require.NoError(t, err, "the fill skips the invalid artifact and embeds locally")
			require.EqualValues(t, 1, requests.Load())
			backlog, err := ix.Backlog(ctx, key)
			require.NoError(t, err)
			require.Zero(t, backlog)
		})
	}
}

// mismatchedInputArtifact builds an internally consistent artifact whose
// identity claims one input while its chunks describe another. The digest
// follows the documented complete-artifact construction.
func mismatchedInputArtifact(t *testing.T, identity embedding.ArtifactIdentity, claimed, actual string) embedding.EmbeddingArtifact {
	t.Helper()
	artifact, err := embedding.NewArtifact(identity, actual, [][]float32{{0, 1}})
	require.NoError(t, err)
	artifact.InputHash = embedding.ArtifactInputHash(claimed)
	manifest := artifact.Manifest()
	manifest.Digest = ""
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	h := sha256.New()
	_, _ = h.Write([]byte("kata.embedding-artifact.v1\x00"))
	sized := func(b []byte) {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(b)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(b)
	}
	sized(raw)
	for _, chunk := range artifact.Chunks {
		sized(chunk.VectorBytes)
	}
	artifact.Digest = hex.EncodeToString(h.Sum(nil))
	require.NoError(t, embedding.ValidateArtifact(artifact, "", ""))
	require.Error(t, embedding.ValidateArtifact(artifact, claimed, ""))
	return artifact
}

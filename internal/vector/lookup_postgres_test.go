package vector_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestPostgresLookupVectorsReadsCoveredChunks(t *testing.T) {
	ctx, store, idx := openLeasedPostgresIndex(t)
	hub, err := store.CreateProject(ctx, "hub")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other")
	require.NoError(t, err)
	body := strings.Repeat("kata ", 1000)
	long, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hub.ID, Title: "long", Body: body, Author: "tester"})
	require.NoError(t, err)
	foreign, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "foreign", Body: "x", Author: "tester"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	gen := kitvec.Generation{Model: "lookup", Dimensions: 2}
	key := gen.Fingerprint()
	require.NoError(t, idx.EnsureBuilding(ctx, key, gen))
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{0.6, 0.8}
		}
		return out, nil
	}
	_, err = idx.Fill(ctx, key, enc, 0, nil, nil)
	require.NoError(t, err)

	got, err := idx.LookupVectors(ctx, key, hub.UID, []string{long.UID, foreign.UID})
	require.NoError(t, err)
	require.Len(t, got, 1, "a foreign project's issue must never be served")
	sv := got[long.UID]
	assert.Equal(t, vector.ContentSHA256(embedding.EmbedText("long", body)), sv.ContentSHA256)
	require.Greater(t, len(sv.Chunks), 1)
	for i, c := range sv.Chunks {
		assert.Equal(t, i, c.ChunkIndex)
		require.Len(t, c.Vector, 2)
		// halfvec storage rounds to float16; the norm stays within the replica
		// validation band.
		assert.InDelta(t, 0.6, c.Vector[0], 1e-3)
		assert.InDelta(t, 0.8, c.Vector[1], 1e-3)
	}

	missing, err := idx.LookupVectors(ctx, "never-ensured", hub.UID, []string{long.UID})
	require.NoError(t, err)
	assert.Empty(t, missing)
}

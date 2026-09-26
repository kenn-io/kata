package vector_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// openLeasedPostgresIndex returns a pgvector-backed store and index holding
// the reconciler lease, so derived-state mutators may run.
func openLeasedPostgresIndex(t *testing.T) (context.Context, *pgstore.Store, *vector.Index) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	idx, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	release, err := idx.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })
	return ctx, store, idx
}

func TestPostgresPendingScopedAndFillExcluding(t *testing.T) {
	ctx, store, idx := openLeasedPostgresIndex(t)
	local, err := store.CreateProject(ctx, "local")
	require.NoError(t, err)
	hub, err := store.CreateProject(ctx, "hub")
	require.NoError(t, err)
	localIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: local.ID, Title: "local", Body: "text", Author: "tester"})
	require.NoError(t, err)
	hubIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hub.ID, Title: "hub", Body: "text", Author: "tester"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	gen := kitvec.Generation{Model: "scoped", Dimensions: 2}
	key := gen.Fingerprint()
	require.NoError(t, idx.EnsureBuilding(ctx, key, gen))
	state, err := idx.GenerationState(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, "building", state)

	only, err := idx.PendingScoped(ctx, key, vector.PendingScope{Projects: []string{hub.UID}, Include: true}, 10)
	require.NoError(t, err)
	require.Len(t, only, 1)
	assert.Equal(t, hubIssue.UID, only[0].Doc)
	assert.Equal(t, hub.UID, only[0].ProjectUID)

	var encoded int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		encoded += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	stats, err := idx.FillExcluding(ctx, key, enc, 0, nil, nil, []string{hub.UID})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Documents)
	assert.Equal(t, 1, encoded)

	rest, err := idx.PendingScoped(ctx, key, vector.PendingScope{}, 10)
	require.NoError(t, err)
	require.Len(t, rest, 1)
	assert.Equal(t, hubIssue.UID, rest[0].Doc)
	assert.NotEqual(t, localIssue.UID, rest[0].Doc)

	err = idx.SaveImported(ctx, key, hubIssue.UID, rest[0].Revision.(int64)+1,
		[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{0, 1}}})
	require.True(t, errors.Is(err, kitvec.ErrStale), "stale revision must not stamp: %v", err)
	require.NoError(t, idx.SaveImported(ctx, key, hubIssue.UID, rest[0].Revision,
		[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{0, 1}}}))
	backlog, err := idx.Backlog(ctx, key)
	require.NoError(t, err)
	assert.Zero(t, backlog)
}

package vector_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// TestPostgresReconcilerLeaseFencesWorkAfterSessionLoss covers fencing when
// the lease session dies, where the driver itself errors. This covers the
// case that has no driver error to lean on: the recorded lease is gone while
// its connection is still healthy and its advisory lock still held. Fencing
// must come from the representation.
func TestPostgresMutatorsFenceAfterTheLeaseSessionIsForgotten(t *testing.T) {
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
	require.NoError(t, idx.EnsureBuilding(ctx, "leased",
		kitvec.Generation{Model: "example", Dimensions: 2}))

	idx.ClearReconcilerLeaseForTest()

	var stillLocked int
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_catalog.pg_locks WHERE `+
			vector.ReconcilerLockPredicateSQL).Scan(&stillLocked))
	require.Equal(t, 1, stillLocked,
		"the advisory lock must still be held, so a driver error cannot be what fences the write")

	require.ErrorIs(t,
		idx.EnsureBuilding(ctx, "unfenced", kitvec.Generation{Model: "example", Dimensions: 2}),
		vector.ErrReconcilerLeaseNotHeldForTest)
	require.ErrorIs(t, idx.ValidateReconcilerLease(ctx),
		vector.ErrReconcilerLeaseNotHeldForTest)

	var rows int
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT count(*) FROM issue_vector_generations WHERE gen_key = 'unfenced'`).Scan(&rows))
	assert.Zero(t, rows, "a forgotten lease must not have written a generation")
}

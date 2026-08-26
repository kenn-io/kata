package vector_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

// The reconciler lock key is written two ways against PostgreSQL: as the two
// int4 arguments pg_try_advisory_lock takes, and as the truncated bigint
// classid/objid pg_locks reports. This asserts both spellings designate the
// same lock, which is the invariant the duplicated literals threatened.
func TestReconcilerLockPredicateMatchesTheAcquiredLock(t *testing.T) {
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

	countLocks := func() int {
		var n int
		require.NoError(t, store.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_catalog.pg_locks WHERE `+
				vector.ReconcilerLockPredicateSQL).Scan(&n))
		return n
	}

	assert.Zero(t, countLocks(), "no lock may match the predicate before one is acquired")

	release, err := idx.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, countLocks(),
		"the pg_locks spelling must match the lock pg_try_advisory_lock took")

	require.NoError(t, release())
	assert.Zero(t, countLocks(), "releasing the lease must drop the matched lock")
}

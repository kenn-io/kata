package vector_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestPostgresReconcilerLeaseFencesWorkAfterSessionLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	first, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	second, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)

	releaseFirst, err := first.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = releaseFirst() })
	var leaderPID int
	require.NoError(t, store.QueryRowContext(ctx, `
		SELECT pid FROM pg_catalog.pg_locks
		WHERE locktype = 'advisory' AND granted
		  AND classid = (hashtext(current_database())::bigint & 4294967295)
		  AND objid = (hashtext('kata:vector:reconciler:' || current_schema())::bigint & 4294967295)
		LIMIT 1`).Scan(&leaderPID))

	secondAcquired := make(chan struct{})
	var releaseSecond func() error
	var secondErr error
	go func() {
		releaseSecond, secondErr = second.AcquireReconcilerLease(ctx)
		close(secondAcquired)
	}()
	select {
	case <-secondAcquired:
		require.FailNow(t, "standby acquired reconciler lease before leader loss")
	case <-time.After(300 * time.Millisecond):
	}

	var terminated bool
	require.NoError(t, store.QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, leaderPID).Scan(&terminated))
	require.True(t, terminated)
	select {
	case <-secondAcquired:
		require.NoError(t, secondErr)
	case <-time.After(3 * time.Second):
		require.FailNow(t, "standby did not acquire reconciler lease after session loss")
	}
	t.Cleanup(func() {
		if releaseSecond != nil {
			_ = releaseSecond()
		}
	})

	err = first.EnsureBuilding(ctx, "stale-leader", kitvec.Generation{Model: "stale", Dimensions: 2})
	require.Error(t, err, "a reconciler must not mutate after its lease session is lost")
	require.NoError(t, second.EnsureBuilding(ctx, "successor", kitvec.Generation{Model: "current", Dimensions: 2}))
	var staleRows int
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT count(*) FROM issue_vector_generations WHERE gen_key = 'stale-leader'`).Scan(&staleRows))
	assert.Zero(t, staleRows)
}

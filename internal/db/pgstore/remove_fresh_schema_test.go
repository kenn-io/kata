package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRemoveFreshSchemaWaitsForServingLease(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	serving, err := pgstore.Open(ctx, dsn, db.Serving())
	require.NoError(t, err)
	instanceUID := serving.InstanceUID()

	result := make(chan error, 1)
	go func() {
		result <- pgstore.RemoveFreshSchema(ctx, dsn, instanceUID)
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		err := serving.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_locks
				WHERE locktype = 'advisory'
				  AND mode = 'ExclusiveLock'
				  AND NOT granted
			)`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "fresh-schema cleanup did not wait for the serving lease")

	version, err := serving.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
	require.NoError(t, serving.Close())
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "fresh-schema cleanup remained blocked after serving stopped")
	}

	version, err = pgstore.PeekSchemaVersion(ctx, dsn)
	require.NoError(t, err)
	assert.Zero(t, version, "fresh schema must be absent after cleanup")
}

func TestRemoveFreshSchemaRefusesChangedIdentityOrDomainState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	instanceUID := store.InstanceUID()

	err = pgstore.RemoveFreshSchema(ctx, dsn, "01HZZZZZZZZZZZZZZZZZZZZZZZ")
	require.Error(t, err)
	version, err := store.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)

	_, err = store.CreateProject(ctx, "concurrent-state")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	err = pgstore.RemoveFreshSchema(ctx, dsn, instanceUID)
	require.Error(t, err)
	version, err = pgstore.PeekSchemaVersion(ctx, dsn)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
}

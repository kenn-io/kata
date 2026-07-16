package pgstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

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

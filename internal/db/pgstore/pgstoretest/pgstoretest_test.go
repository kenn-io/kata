package pgstoretest_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/testenv"
)

// Clones share the installed schema but not the instance identity, which
// federation and relay tests rely on to tell two stores apart.
func TestNewDatabaseClonesSchemaWithFreshIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	open := func(t *testing.T) *pgstore.Store {
		t.Helper()
		dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
		t.Cleanup(cleanup)
		store, err := pgstore.Open(t.Context(), dsn)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		return store
	}
	first, second := open(t), open(t)
	require.NotEmpty(t, first.InstanceUID())
	require.NotEqual(t, first.InstanceUID(), second.InstanceUID())
	for _, store := range []*pgstore.Store{first, second} {
		createdAt, err := store.InstanceCreatedAt(t.Context())
		require.NoError(t, err)
		require.False(t, createdAt.IsZero())
		_, err = store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err, "each clone has its own copy of the schema")
	}
}

func TestNewTemplateInstallsNamedSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	dsn, cleanup := pgstoretest.NewTemplate("example_schema").NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	config := pgstore.DefaultConfig()
	config.Schema = "example_schema"
	store, err := pgstore.OpenWithConfig(t.Context(), dsn, config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.False(t, store.InstalledFreshSchema(), "the clone already holds the schema")
	require.NotEmpty(t, store.InstanceUID())
}

// Some tests compare row IDs across stores, so a clone's first open must
// leave the same rows and IDs as opening an empty database.
func TestNewDatabaseMatchesFreshInstallIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	ids := func(t *testing.T, newDatabase func(*testing.T, context.Context) (string, func())) [2]int64 {
		t.Helper()
		dsn, cleanup := newDatabase(t, t.Context())
		t.Cleanup(cleanup)
		store, err := pgstore.Open(t.Context(), dsn)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		system, err := store.SystemProject(t.Context())
		require.NoError(t, err)
		project, err := store.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		return [2]int64{system.ID, project.ID}
	}
	fresh := ids(t, testenv.NewPostgresContainer)
	clone := ids(t, pgstoretest.NewDatabase)
	require.Equal(t, fresh, clone)
}

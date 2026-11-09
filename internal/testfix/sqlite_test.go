package testfix_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testfix"
)

func TestOpenSQLiteFreshProjectIDs(t *testing.T) {
	ctx := t.Context()
	fresh, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	cloned, err := testfix.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cloned.Close()) })
	second, err := testfix.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NotEqual(t, cloned.InstanceUID(), second.InstanceUID())
	wantSystem, err := fresh.SystemProject(ctx)
	require.NoError(t, err)
	gotSystem, err := cloned.SystemProject(ctx)
	require.NoError(t, err)
	require.Equal(t, wantSystem.ID, gotSystem.ID)
	wantProject, err := fresh.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	gotProject, err := cloned.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	require.Equal(t, wantProject.ID, gotProject.ID)
}

func TestOpenSQLitePreservesExistingDatabase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, err := testfix.OpenSQLite(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	uid := store.InstanceUID()
	require.NoError(t, store.Close())
	reopened, err := testfix.OpenSQLite(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	got, err := reopened.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, project, got)
	require.Equal(t, uid, reopened.InstanceUID())
}

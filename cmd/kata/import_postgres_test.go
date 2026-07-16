package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/testenv"
)

func TestImportPostgresTargetCreatesThenAtomicallyReplacesSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	input := writeExportFixture(t, home)
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	wantTarget, err := config.CanonicalDSNIdentity(dsn)
	require.NoError(t, err)

	out, err := runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	require.NoError(t, err)
	assert.Contains(t, out, wantTarget)
	assert.NotContains(t, out, "kata:kata@",
		"success output must not expose Postgres userinfo")

	store, err := storeopen.Open(ctx, dsn)
	require.NoError(t, err)
	project, err := store.ProjectByName(ctx, "kata")
	require.NoError(t, err)
	issues, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "imported issue", issues[0].Title)
	extra, err := store.CreateProject(ctx, "must-be-replaced")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	_, err = runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "target already exists")

	_, err = runCmdOutput(t, nil, "import", "--force", "--input", input, "--target", dsn)
	require.NoError(t, err)
	restored, err := storeopen.OpenReadOnly(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restored.Close() })
	_, err = restored.ProjectByUID(ctx, extra.UID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportPostgresFailureRemovesFreshTargetSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	input := filepath.Join(home, "invalid.jsonl")
	require.NoError(t, os.WriteFile(input, []byte(`{"kind":"issue","data":{}}`+"\n"), 0o600))
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_POSTGRES_SCHEMA", "restore_store")

	_, err := runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	require.Error(t, err)
	version, peekErr := storeopen.PeekSchemaVersion(ctx, dsn)
	require.NoError(t, peekErr)
	assert.Zero(t, version, "failed fresh import must remove the target schema")
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'restore_store')`).Scan(&exists))
	assert.False(t, exists, "cleanup must remove the configured target schema")
}

func TestImportPostgresFailurePreservesPreexistingEmptySchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	input := filepath.Join(home, "invalid.jsonl")
	require.NoError(t, os.WriteFile(input, []byte(`{"kind":"issue","data":{}}`+"\n"), 0o600))
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_POSTGRES_SCHEMA", "preexisting_store")
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `CREATE SCHEMA preexisting_store`)
	require.NoError(t, err)

	_, err = runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	require.Error(t, err)

	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'preexisting_store')`).Scan(&exists))
	assert.True(t, exists, "failed import must not drop a schema it did not create")
}

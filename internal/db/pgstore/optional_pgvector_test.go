package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestPostgresBootstrapDoesNotInstallAvailablePgvector(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var databaseName string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName))
	var vectorAvailable, vectorInstalled bool
	require.NoError(t, admin.QueryRowContext(ctx, `
		SELECT EXISTS (
		         SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name='vector'
		       ),
		       EXISTS (
		         SELECT 1 FROM pg_catalog.pg_extension WHERE extname='vector'
		       )`).Scan(&vectorAvailable, &vectorInstalled))
	require.True(t, vectorAvailable, "test requires a server that offers pgvector")
	require.False(t, vectorInstalled, "test requires pgvector to start uninstalled")

	roleName := fmt.Sprintf("optional_vector_owner_%d", os.Getpid())
	role := pgx.Identifier{roleName}.Sanitize()
	database := pgx.Identifier{databaseName}.Sanitize()
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		CREATE EXTENSION IF NOT EXISTS unaccent WITH SCHEMA public;
		CREATE ROLE %s LOGIN PASSWORD 'runtime-password';
		GRANT CREATE ON DATABASE %s TO %s;
	`, role, database, role)) // #nosec G201 -- identifiers are sanitized above.
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP OWNED BY "+role)
		_, _ = admin.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
	})

	store, err := pgstore.OpenWithConfig(
		ctx,
		postgresDSNWithUser(t, dsn, roleName, "runtime-password"),
		pgstore.Config{Schema: "optional_vector_store", SchemaMode: pgstore.SchemaModeBootstrap},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	created, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "core postgres remains usable",
		Author:    "operator",
	})
	require.NoError(t, err)
	issues, err := store.SearchFTS(ctx, project.ID, "core postgres", 10, false)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, created.UID, issues[0].Issue.UID)

	require.NoError(t, admin.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM pg_catalog.pg_extension WHERE extname='vector'
		)`).Scan(&vectorInstalled))
	assert.False(t, vectorInstalled, "bootstrap must not install optional database extensions")
}

func TestPostgresBootstrapDoesNotRequirePgvector(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPlainPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	created, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "plain postgres remains usable",
		Author:    "operator",
	})
	require.NoError(t, err)

	issues, err := store.SearchFTS(ctx, project.ID, "plain postgres", 10, false)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, created.UID, issues[0].Issue.UID)
}

func TestPostgresBootstrapDoesNotRequireHalfvec(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `
		ALTER EXTENSION vector DROP TYPE public.halfvec;
		DROP TYPE public.halfvec CASCADE;
	`)
	require.NoError(t, err)

	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "older-vector-project")
	require.NoError(t, err)
	created, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "core storage without halfvec",
		Author:    "operator",
	})
	require.NoError(t, err)
	issues, err := store.SearchFTS(ctx, project.ID, "without halfvec", 10, false)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, created.UID, issues[0].Issue.UID)

	var vectorTables int
	require.NoError(t, admin.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM pg_catalog.pg_class relation
		  JOIN pg_catalog.pg_namespace namespace ON namespace.oid=relation.relnamespace
		 WHERE namespace.nspname='kata'
		   AND relation.relname IN ('vector_generations', 'issue_vector_mirror', 'vector_chunks')
	`).Scan(&vectorTables))
	assert.Zero(t, vectorTables, "bootstrap must leave semantic storage unavailable without halfvec")
}

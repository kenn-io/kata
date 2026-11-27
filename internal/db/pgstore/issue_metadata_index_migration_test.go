package pgstore_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// dropSchema56IssueMetadataIndex returns a fresh schema's issue indexes to
// their schema 55 shape.
func dropSchema56IssueMetadataIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_issues_metadata`, schema)) // #nosec G201 -- schema is a fixed test identifier.
	require.NoError(t, err)
}

// issueMetadataIndex returns the definition of idx_issues_metadata in schema,
// or "" when the schema has none.
func issueMetadataIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) string {
	t.Helper()
	var definition sql.NullString
	err := admin.QueryRowContext(ctx, `SELECT replace(indexdef,$1||'.','') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_issues_metadata'`, schema).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return definition.String
}

// Contract: schema 55 upgrades to 56 by adding the GIN index on issue
// metadata, matching a fresh install, and a key-only metadata filter across
// projects still returns the issue that has the key.
func TestIssueMetadataIndexMigrationUpgradesVersion55(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "issue_metadata_index_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "metadata-upgrade")
	require.NoError(t, err)
	flagged, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Flagged", Author: "worker",
		Metadata: map[string]jsontext.Value{"work.attention": jsontext.Value(`"stuck"`)},
	})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Plain", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropSchema56IssueMetadataIndex(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE issue_metadata_index_upgrade.meta SET value='55' WHERE key='schema_version'`)
	require.NoError(t, err)

	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
	issues, err := upgraded.ListAllIssues(ctx, db.ListAllIssuesParams{
		CreatedFirst: true,
		Meta:         []db.MetaFilter{{Key: "work.attention"}},
	})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, flagged.ID, issues[0].ID)

	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "issue_metadata_index_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	migrated := issueMetadataIndex(ctx, t, admin, schema)
	assert.Equal(t, issueMetadataIndex(ctx, t, admin, "issue_metadata_index_fresh"), migrated, "migrated and fresh schemas must agree")
	assert.Equal(t, "CREATE INDEX idx_issues_metadata ON issues USING gin (((metadata)::jsonb))", migrated)
}

package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// Older-version fixtures start from the current schema and remove later indexes.
func dropLaterMigrationIndexes(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_comments_uid_handle`, schema)) // #nosec G201 -- Fixed test schema.
	require.NoError(t, err)
	dropIssueCreatedAndEventIndexes(ctx, t, admin, schema)
	dropMCPEventSubscriptionsSchema(ctx, t, admin, schema)
}

func dropIssueCreatedAndEventIndexes(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_issues_project_created; DROP INDEX IF EXISTS %s.idx_issues_created; DROP INDEX IF EXISTS %s.idx_events_resolved_issue_uid; DROP INDEX IF EXISTS %s.idx_events_reset_from; DROP INDEX IF EXISTS %s.idx_issues_uid_prefix; DROP INDEX IF EXISTS %s.idx_issues_project_uid_prefix`, schema, schema, schema, schema, schema, schema)) // #nosec G201 -- Fixed test schema.
	require.NoError(t, err)
	dropLaterReadyAndEventIndexes(ctx, t, admin, schema)
}

func TestCommentUIDHandleIndexMigrationUpgradesVersion38(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "comment_handle_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropLaterMigrationIndexes(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE comment_handle_upgrade.meta SET value='38' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "comment_handle_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	for _, targetSchema := range []string{schema, "comment_handle_fresh"} {
		var definition string
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_comments_uid_handle'`, targetSchema).Scan(&definition))
		require.Equal(t, `CREATE INDEX idx_comments_uid_handle ON comments USING btree (lower(substr(uid, 21)))`, definition)
	}
}

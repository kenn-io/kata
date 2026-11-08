package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssueCreatedOrderIndexMigrationUpgradesVersion41(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "issue_created_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropIssueCreatedAndEventIndexes(ctx, t, admin, schema)
	dropMCPEventSubscriptionsSchema(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE issue_created_upgrade.meta SET value='41' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "issue_created_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	for _, targetSchema := range []string{schema, "issue_created_fresh"} {
		for name, columns := range map[string]string{
			"idx_issues_project_created": "project_id, created_at, id",
			"idx_issues_created":         "created_at, id",
		} {
			var definition string
			require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname=$2`, targetSchema, name).Scan(&definition))
			require.Equal(t, "CREATE INDEX "+name+" ON issues USING btree ("+columns+") WHERE (deleted_at IS NULL)", definition)
		}
	}
}

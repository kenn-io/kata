package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestEventMovedFromIDIndexMigrationUpgradesVersion46(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "event_moved_from_id_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `DROP INDEX event_moved_from_id_upgrade.idx_events_moved_from_id;
UPDATE event_moved_from_id_upgrade.meta SET value='46' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "event_moved_from_id_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	for _, targetSchema := range []string{schema, "event_moved_from_id_fresh"} {
		var definition string
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_events_moved_from_id'`, targetSchema).Scan(&definition))
		require.Equal(t, `CREATE INDEX idx_events_moved_from_id ON events USING btree ((((payload)::jsonb ->> 'from_project_uid'::text)), id) WHERE (type = 'issue.moved'::text)`, definition)
	}
}

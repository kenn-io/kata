package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func dropMCPEventSubscriptionsSchema(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s.mcp_event_subscriptions`, schema)) // #nosec G201 -- schema is a fixed test identifier.
	require.NoError(t, err)
}

// mcpEventSubscriptionsShape lists the columns, constraints and indexes the
// schema 35 migration owns, so a fresh and a migrated schema compare exactly.
func mcpEventSubscriptionsShape(ctx context.Context, t *testing.T, admin *sql.DB, schema string) []string {
	t.Helper()
	rows, err := admin.QueryContext(ctx, `SELECT value FROM (
 SELECT 'constraint:' || c.relname || ':' || con.conname || ':' || replace(pg_get_constraintdef(con.oid,true),$1||'.','') AS value FROM pg_constraint con JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
 UNION ALL SELECT 'index:' || tablename || ':' || indexname || ':' || replace(indexdef,$1||'.','') FROM pg_indexes WHERE schemaname=$1
 UNION ALL SELECT 'column:' || table_name || ':' || column_name || ':' || ordinal_position || ':' || udt_name || ':' || is_nullable || ':' || is_identity || ':' || replace(COALESCE(column_default,''),$1||'.','') FROM information_schema.columns WHERE table_schema=$1
 ) shapes WHERE value ~ '^(column|constraint|index):mcp_event_subscriptions:'
 ORDER BY value`, schema)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}

func TestMCPEventSubscriptionsMigrationMatchesFreshSchema(t *testing.T) {
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "mcp_events_upgrade"
	source, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropMCPEventSubscriptionsSchema(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE mcp_events_upgrade.meta SET value='34' WHERE key='schema_version'`)
	require.NoError(t, err)

	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, 35, version)
	assert.Equal(t, db.CurrentSchemaVersion(), version)

	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "mcp_events_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })

	freshShape := mcpEventSubscriptionsShape(ctx, t, admin, "mcp_events_fresh")
	require.NotEmpty(t, freshShape)
	assert.Equal(t, freshShape, mcpEventSubscriptionsShape(ctx, t, admin, schema), "migrated and fresh schemas must agree")
	assert.Contains(t, freshShape, "index:mcp_event_subscriptions:idx_mcp_event_subscriptions_due:CREATE INDEX idx_mcp_event_subscriptions_due ON mcp_event_subscriptions USING btree (state, next_attempt_at)")

	var empty int
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_event_subscriptions`).Scan(&empty))
	assert.Zero(t, empty)
}

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

func TestReadyIndexMigrationUpgrades(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	for _, sourceVersion := range []int{45, 46, 48} {
		t.Run(fmt.Sprint(sourceVersion), func(t *testing.T) {
			ctx := t.Context()
			dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
			t.Cleanup(cleanup)
			const schema = "ready_priority_upgrade"
			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
			require.NoError(t, err)
			require.NoError(t, store.Close())
			admin, err := sql.Open("pgx", dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = admin.Close() })
			if sourceVersion == 45 {
				dropLaterReadyAndEventIndexes(ctx, t, admin, schema)
			} else if sourceVersion == 46 {
				_, err = admin.ExecContext(ctx, `DROP INDEX IF EXISTS ready_priority_upgrade.idx_issues_ready_updated; DROP INDEX ready_priority_upgrade.idx_events_moved_from_id`)
				require.NoError(t, err)
			}
			if sourceVersion != 45 {
				_, err = admin.ExecContext(ctx, `DROP INDEX IF EXISTS ready_priority_upgrade.idx_events_cron_run_uid`)
				require.NoError(t, err)
				dropMCPEventSubscriptionsSchema(ctx, t, admin, schema)
			}
			_, err = admin.ExecContext(ctx, `UPDATE ready_priority_upgrade.meta SET value=$1 WHERE key='schema_version'`, fmt.Sprint(sourceVersion))
			require.NoError(t, err)
			upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
			require.NoError(t, err)
			t.Cleanup(func() { _ = upgraded.Close() })
			version, err := upgraded.SchemaVersion(ctx)
			require.NoError(t, err)
			require.Equal(t, db.CurrentSchemaVersion(), version)
			fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "ready_priority_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
			require.NoError(t, err)
			require.NoError(t, fresh.Close())
			for _, targetSchema := range []string{schema, "ready_priority_fresh"} {
				var definition string
				require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_events_cron_run_uid'`, targetSchema).Scan(&definition))
				require.Equal(t, `CREATE INDEX idx_events_cron_run_uid ON events USING btree (project_id, type, (((payload)::jsonb ->> 'uid'::text))) WHERE (type ~~ 'cron.%'::text)`, definition)
				require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_events_moved_from_id'`, targetSchema).Scan(&definition))
				require.Equal(t, `CREATE INDEX idx_events_moved_from_id ON events USING btree ((((payload)::jsonb ->> 'from_project_uid'::text)), id) WHERE (type = 'issue.moved'::text)`, definition)
				for name, columns := range map[string]string{
					"idx_issues_project_ready_priority": "project_id, COALESCE(priority, 5), updated_at DESC, id DESC",
					"idx_issues_ready_priority":         "COALESCE(priority, 5), updated_at DESC, id DESC",
					"idx_issues_ready_updated":          "updated_at DESC, id DESC",
				} {
					var definition string
					require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname=$2`, targetSchema, name).Scan(&definition))
					require.Equal(t, "CREATE INDEX "+name+" ON issues USING btree ("+columns+") WHERE ((status = 'open'::text) AND (deleted_at IS NULL))", definition)
				}
			}
		})
	}
}

func dropLaterReadyAndEventIndexes(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_issues_project_ready_priority; DROP INDEX IF EXISTS %s.idx_issues_ready_priority; DROP INDEX IF EXISTS %s.idx_issues_ready_updated; DROP INDEX IF EXISTS %s.idx_events_moved_from_id; DROP INDEX IF EXISTS %s.idx_events_cron_run_uid`, schema, schema, schema, schema, schema)) // #nosec G201 -- Fixed test schema.
	require.NoError(t, err)
	dropMCPEventSubscriptionsSchema(ctx, t, admin, schema)
}

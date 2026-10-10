package pgstore_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"go.kenn.io/kata/internal/cron"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func dropNativeCronSchema(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	for _, table := range []string{"cron_runs", "cron_jobs", "cron_workflows"} {
		_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s`, schema, table))
		require.NoError(t, err)
	}
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_events_project_cron`, schema))
	require.NoError(t, err)
}

func TestNativeCronMigrationPreservesVersion30(t *testing.T) {
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "cron_upgrade"
	source, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	owner := "worker"
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained task", Author: "worker", Owner: &owner, Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-10-01T09:00"`)}})
	require.NoError(t, err)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	due, err := source.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{IssueID: issue.ID, Now: now, DefaultTimezone: "UTC"})
	require.NoError(t, err)
	require.True(t, due.Changed)
	issue, err = source.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropCommentReplySchema(ctx, t, admin, schema)
	dropNativeCronSchema(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE cron_upgrade.meta SET value='30' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.Equal(t, "296b297d7f2e06a90d7d93e89fc851903eba4a8d7d41316aca0a4db6dca8e103", columnFingerprint(ctx, t, admin, schema))
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	var count int
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&count))
	require.Zero(t, count)
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 32, version)
	got, err := upgraded.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue, got)
	again, err := upgraded.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{IssueID: issue.ID, Now: now, DefaultTimezone: "UTC"})
	require.NoError(t, err)
	require.False(t, again.Changed)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := upgraded.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: definition, Actor: "worker"})
	require.NoError(t, err)
	restored, err := upgraded.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, job, restored)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "cron_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	require.Equal(t, columnFingerprint(ctx, t, admin, "cron_fresh"), columnFingerprint(ctx, t, admin, schema))
	require.Equal(t, cronPhysicalShape(ctx, t, admin, "cron_fresh"), cronPhysicalShape(ctx, t, admin, schema))
}

func cronPhysicalShape(ctx context.Context, t *testing.T, admin *sql.DB, schema string) []string {
	t.Helper()
	rows, err := admin.QueryContext(ctx, `SELECT value FROM (
 SELECT 'constraint:' || c.relname || ':' || con.conname || ':' || replace(pg_get_constraintdef(con.oid,true),$1||'.','') AS value FROM pg_constraint con JOIN pg_class c ON c.oid=con.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1
 UNION ALL SELECT 'index:' || tablename || ':' || indexname || ':' || replace(indexdef,$1||'.','') FROM pg_indexes WHERE schemaname=$1
 UNION ALL SELECT 'column:' || table_name || ':' || column_name || ':' || ordinal_position || ':' || udt_name || ':' || is_nullable || ':' || is_identity || ':' || replace(COALESCE(column_default,''),$1||'.','') FROM information_schema.columns WHERE table_schema=$1
 ) shapes WHERE value ~ '^(column|constraint|index):cron_' OR value LIKE 'index:events:idx_events_project_cron:%'
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

func TestNativeCronMigrationRollback(t *testing.T) {
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "cron_rollback"
	source, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropNativeCronSchema(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE cron_rollback.meta SET value='30' WHERE key='schema_version'; CREATE TABLE cron_rollback.cron_runs(blocker INTEGER)`)
	require.NoError(t, err)
	failed, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.ErrorContains(t, err, "apply postgres migration 000031_native_cron.up.sql")
	require.Nil(t, failed)
	var version string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT value FROM cron_rollback.meta WHERE key='schema_version'`).Scan(&version))
	require.Equal(t, "30", version)
	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT to_regclass('cron_rollback.cron_jobs') IS NOT NULL`).Scan(&exists))
	require.False(t, exists, "all earlier statements roll back with the version stamp")
}

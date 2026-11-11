package pgstore_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRecurringDeliveryKeyMatchesDecoder(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "recurring_delivery_key", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, occurrence := range []string{
		`{"version":1,"scheduled_at":"2025-01-01T00:00:00.1234567899Z"}`,
		`{"version":1,"scheduled_at":"2025-01-01T00:00:00.123456788Z"}`,
		`{"version":1,"scheduled_at":"2025-01-01T00:00:00.5+01:30"}`,
		`{"version":1,"scheduled_at":"2024-12-31T22:30:00.500000000Z"}`,
		`{"version":1,"scheduled_at":"0001-01-01T00:00:00Z"}`,
		`{"version":1,"scheduled_at":"0000-02-29T23:59:59.1-03:00"}`,
		`{"version":1,"scheduled_at":"2025-01-01T1:00:00Z"}`,
		`{"version":1,"scheduled_at":"2025-01-01T01:00:00,01Z"}`,
		`{"version":1,"scheduled_at":"2025-01-01T00:00:00+24:00"}`,
		`{"version":1,"scheduled_at":"2025-01-01T00:00:00+00:60"}`,
		`{"version":1,"scheduled_at":"2025-02-30T00:00:00Z"}`,
		`{"version":1,"scheduled_at":null}`,
		`{"version":"2","scheduled_at":"2025-01-01T00:00:00Z"}`,
		`{"version":2,"scheduled_at":false}`,
		`{"version":9999999999999999999999999}`,
		`{"version":9223372036854775808}`,
		`{"version":1e0,"scheduled_at":"2025-01-01T00:00:00Z"}`,
		`{"version":1.0,"scheduled_at":"2025-01-01T00:00:00Z"}`,
		`{"version":0}`,
		`{"scheduled_at":"2025-01-01T00:00:00Z"}`,
		`{"version":1,"scheduled_at":"2025-01-01T24:00:00Z"}`,
		`{"version":1,"scheduled_at":"2025-01-01T23:59:60Z"}`,
	} {
		var got sql.NullString
		require.NoError(t, store.QueryRowContext(ctx, `SELECT kata_occurrence_key($1::json)`, occurrence).Scan(&got))
		require.Equal(t, db.RecurringOccurrenceKey(occurrence), got.String, occurrence)
	}
}

func TestRecurringDeliveryMigrationPostgres(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "recurring_delivery_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)

	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Parent", Author: "operator"})
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Recurring", Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 900}, Action: cron.Action{Kind: "notify", Recipient: "operator"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Work"}, Overlap: "allow", Catchup: "all"}})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Child", Author: "operator"})
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "operator"})
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, fmt.Sprintf(`{"cron.occurrence":{"version":1,"job_uid":%q,"scheduled_at":"2026-01-01T00:00:00Z"}}`, job.UID), child.ID)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	_, err = admin.ExecContext(ctx, `DROP INDEX recurring_delivery_upgrade.idx_issues_recurring_delivery; DROP FUNCTION recurring_delivery_upgrade.kata_occurrence_key(json); UPDATE recurring_delivery_upgrade.meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	var indexExists bool
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT to_regclass('recurring_delivery_upgrade.idx_issues_recurring_delivery') IS NOT NULL`).Scan(&indexExists))
	require.True(t, indexExists)
	rows, err := upgraded.RecurringCronDelivery(ctx, db.CronDeliveryQuery{ProjectID: p.ID, JobUID: job.UID}, time.Now())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Delivery.LastChild)
	require.Equal(t, child.UID, rows[0].Delivery.LastChild.UID)
}

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
	rows, err := upgraded.RecurringCronDelivery(ctx, db.CronDeliveryQuery{ProjectID: p.ID, JobUID: job.UID}, time.Now())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Delivery.LastChild)
	require.Equal(t, child.UID, rows[0].Delivery.LastChild.UID)
}

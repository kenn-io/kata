package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRecurringDeliveryMigrationSQLite(t *testing.T) {
	store, path := openTestDBWithPath(t)
	ctx := t.Context()

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
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(ctx, `DROP INDEX idx_issues_recurring_delivery; UPDATE meta SET value='50' WHERE key='schema_version'; CREATE TABLE idx_issues_recurring_delivery(x INTEGER)`)
	require.NoError(t, err)
	require.Error(t, sqlitestore.UpgradeInPlace(ctx, path))
	var version int
	require.NoError(t, raw.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&version))
	require.Equal(t, 50, version)
	_, err = raw.ExecContext(ctx, `DROP TABLE idx_issues_recurring_delivery`)
	require.NoError(t, err)
	require.NoError(t, sqlitestore.UpgradeInPlace(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	version, err = upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	rows, err := upgraded.RecurringCronDelivery(ctx, db.CronDeliveryQuery{ProjectID: p.ID, JobUID: job.UID}, time.Now())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].Delivery.LastChild)
	require.Equal(t, child.UID, rows[0].Delivery.LastChild.UID)

	var key sql.NullString
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT kata_occurrence_key('{"version":1,"scheduled_at":"2026-01-01T00:00:00Z"}')`).Scan(&key))
	require.True(t, key.Valid)
}

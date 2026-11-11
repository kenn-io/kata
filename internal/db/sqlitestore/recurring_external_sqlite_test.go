package sqlitestore_test

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRecurringDeliveryExternalSQLite(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 executable unavailable")
	}
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("upgrade=%t", upgrade), func(t *testing.T) {
			ctx := t.Context()
			store, path := openTestDBWithPath(t)
			p, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Parent", Author: "operator"})
			require.NoError(t, err)
			job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Recurring", Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 900}, Action: cron.Action{Kind: "notify", Recipient: "operator"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Work"}, Overlap: "allow", Catchup: "latest"}})
			require.NoError(t, err)
			_, err = store.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: time.Now().Add(time.Hour)})
			require.NoError(t, err)
			children, err := store.ChildrenOfIssue(ctx, parent.ID)
			require.NoError(t, err)
			require.Len(t, children, 1)
			if upgrade {
				_, err = store.ExecContext(ctx, `DROP INDEX idx_issues_recurring_delivery; UPDATE meta SET value='50' WHERE key='schema_version'`)
				require.NoError(t, err)
			}
			require.NoError(t, store.Close())
			if upgrade {
				require.NoError(t, sqlitestore.UpgradeInPlace(ctx, path))
			}
			run := func(database, command string) string {
				t.Helper()
				out, err := exec.CommandContext(ctx, sqlite, database, command).CombinedOutput()
				require.NoError(t, err, "%s: %s", command, out)
				return string(out)
			}
			require.Equal(t, "ok\n", run(path, "PRAGMA integrity_check;"))
			run(path, "REINDEX;")
			run(path, fmt.Sprintf(`INSERT INTO issues(uid,project_id,short_id,title,author) VALUES('00000000000000000000000001',%d,'000001','Extra','operator')`, p.ID))
			run(path, `UPDATE issues SET metadata='{"cron.occurrence":"not an object"}' WHERE uid='00000000000000000000000001'`)
			run(path, fmt.Sprintf(`UPDATE issues SET metadata=json_set(metadata,'$."cron.occurrence".scheduled_at','2025-01-01T00:00:00.123456789Z') WHERE id=%d`, children[0].ID))
			backup := filepath.Join(t.TempDir(), "backup.db")
			run(path, fmt.Sprintf(".backup %q", backup))
			require.Equal(t, "ok\n", run(backup, "PRAGMA integrity_check;"))
			reopened, err := sqlitestore.Open(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			rows, err := reopened.RecurringCronDelivery(ctx, db.CronDeliveryQuery{ProjectID: p.ID, JobUID: job.UID}, time.Now())
			require.NoError(t, err)
			require.Equal(t, children[0].UID, rows[0].Delivery.LastChild.UID)
			require.Equal(t, "2025-01-01T00:00:00.123456789Z", rows[0].Delivery.LastChild.ScheduledAt.Format(time.RFC3339Nano))
		})
	}
}

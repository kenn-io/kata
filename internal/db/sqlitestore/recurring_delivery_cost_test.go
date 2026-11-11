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

func TestRecurringDeliveryHistoryCost(t *testing.T) {
	store, path := openTestDBWithPath(t)
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Recurring parent", Author: "operator"})
	require.NoError(t, err)
	jobs := make([]db.CronJob, 4)
	for i := range jobs {
		jobs[i], _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: fmt.Sprintf("Job %d", i), Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 900}, Action: cron.Action{Kind: "notify", Recipient: "operator"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Work"}, Overlap: "allow", Catchup: "all"}})
		require.NoError(t, err)
	}
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	reader := &sqlitestore.Store{DB: raw}
	count := func(query db.CronDeliveryQuery) int {
		conn, err := raw.Conn(ctx)
		require.NoError(t, err)
		sqlitePageFetches(t, conn, true)
		require.NoError(t, conn.Close())
		result, err := reader.RecurringCronDelivery(ctx, query, time.Now())
		require.NoError(t, err)
		require.NotEmpty(t, result)
		conn, err = raw.Conn(ctx)
		require.NoError(t, err)
		pages := sqlitePageFetches(t, conn, false)
		require.NoError(t, conn.Close())
		return pages
	}
	var smallShow, smallList int
	for _, n := range []int{1000, 35040} {
		start := 1
		if n > 1000 {
			start = 1001
		}
		for i, j := range jobs {
			_, err = store.ExecContext(ctx, `WITH RECURSIVE seq(n) AS (VALUES(?) UNION ALL SELECT n+1 FROM seq WHERE n<?) INSERT INTO issues(uid,project_id,short_id,title,author,metadata) SELECT printf('%026d',n+?),?,printf('%08d',n+?),'Work','operator',json_object('cron.occurrence',json_object('version',1,'job_uid',?,'scheduled_at',strftime('%Y-%m-%dT%H:%M:%SZ','2025-01-01',printf('+%d minutes',n*15)))) FROM seq`, start, n, (i+1)*10000000, p.ID, (i+1)*10000000, j.UID)
			require.NoError(t, err)
		}
		_, err = store.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author) SELECT id,?,uid,?,'parent','operator' FROM issues WHERE project_id=? AND id<>? AND NOT EXISTS(SELECT 1 FROM links WHERE from_issue_id=issues.id AND type='parent')`, parent.ID, parent.UID, p.ID, parent.ID)
		require.NoError(t, err)
		show := count(db.CronDeliveryQuery{ProjectID: p.ID, JobUID: jobs[0].UID})
		list := count(db.CronDeliveryQuery{ProjectID: p.ID})
		t.Logf("%d children per job: show=%d pages, list=%d pages", n, show, list)
		if n == 1000 {
			smallShow, smallList = show, list
		} else {
			require.LessOrEqual(t, show, smallShow+100, "show cost should stay near the smaller history")
			require.LessOrEqual(t, list, smallList+400, "list cost should stay near the smaller history")
		}
	}
}

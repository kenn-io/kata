package pgstore_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRecurringDeliveryHistoryCostPostgres(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "delivery_cost", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Recurring parent", Author: "operator"})
	require.NoError(t, err)
	jobs := make([]db.CronJob, 4)
	for i := range jobs {
		jobs[i], _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: fmt.Sprintf("Job %d", i), Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 900}, Action: cron.Action{Kind: "notify", Recipient: "operator"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Work"}, Overlap: "allow", Catchup: "all"}})
		require.NoError(t, err)
	}
	for _, n := range []int{1000, 35040} {
		start := 1
		if n > 1000 {
			start = 1001
		}
		for i, j := range jobs {
			_, err = store.ExecContext(ctx, `INSERT INTO issues(uid,project_id,short_id,title,author,metadata) SELECT lpad((n+$3)::text,26,'0'),$1,lpad((n+$3)::text,8,'0'),'Work','operator',json_build_object('cron.occurrence',json_build_object('version',1,'job_uid',$4::text,'scheduled_at',to_char(timestamp '2025-01-01' + n * interval '15 minutes','YYYY-MM-DD"T"HH24:MI:SS"Z"')))::text FROM generate_series($2::integer,$5::integer) n`, p.ID, start, (i+1)*10000000, j.UID, n)
			require.NoError(t, err)
		}
		_, err = store.ExecContext(ctx, `INSERT INTO links(from_issue_id,to_issue_id,from_issue_uid,to_issue_uid,type,author) SELECT id,$1,uid,$2,'parent','operator' FROM issues WHERE project_id=$3 AND id<>$1 AND NOT EXISTS(SELECT 1 FROM links WHERE from_issue_id=issues.id AND type='parent')`, parent.ID, parent.UID, p.ID)
		require.NoError(t, err)
		_, err = store.ExecContext(ctx, `ANALYZE issues; ANALYZE links`)
		require.NoError(t, err)
		for _, query := range []db.CronDeliveryQuery{{ProjectID: p.ID, JobUID: jobs[0].UID}, {ProjectID: p.ID}} {
			startTime := time.Now()
			for range 3 {
				result, err := store.RecurringCronDelivery(ctx, query, time.Now())
				require.NoError(t, err)
				require.NotEmpty(t, result)
			}
			t.Logf("%d children per job, job=%t: three reads %s", n, query.JobUID != "", time.Since(startTime))
		}
		var raw []byte
		err = store.QueryRowContext(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT i.id, i.uid, i.metadata::jsonb -> 'cron.occurrence' FROM issues i WHERE (i.metadata::jsonb -> 'cron.occurrence' ->> 'job_uid') COLLATE "C"=$1 AND i.deleted_at IS NULL AND kata_occurrence_key(i.metadata::jsonb -> 'cron.occurrence') IS NOT NULL AND EXISTS (SELECT 1 FROM links l JOIN projects p ON p.id=i.project_id WHERE l.from_issue_id=i.id AND l.type='parent' AND l.to_issue_id=$2 AND p.deleted_at IS NULL) ORDER BY kata_occurrence_key(i.metadata::jsonb -> 'cron.occurrence') COLLATE "C" DESC, i.uid COLLATE "C" DESC LIMIT 1`, jobs[0].UID, parent.ID).Scan(&raw)
		require.NoError(t, err)
		var result []struct {
			Plan map[string]any `json:"Plan"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		var findIndex func(map[string]any) (float64, bool)
		findIndex = func(node map[string]any) (float64, bool) {
			if node["Index Name"] == "idx_issues_recurring_delivery" {
				return node["Actual Rows"].(float64), true
			}
			if children, ok := node["Plans"].([]any); ok {
				for _, child := range children {
					if rows, found := findIndex(child.(map[string]any)); found {
						return rows, true
					}
				}
			}
			return 0, false
		}
		rows, found := findIndex(result[0].Plan)
		require.True(t, found, "lookup should use occurrence index: %s", raw)
		require.LessOrEqual(t, rows, float64(4), "lookup should avoid retained history: %s", raw)
	}
}

package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Each history contains one retained observation and one current projection per
// v1 run, with a 1 KiB message. Run with -benchtime=1x; setup is not timed.
func BenchmarkProjectSummaryFeatureScan(b *testing.B) {
	for _, history := range []int{1000, 10000, 50000} {
		b.Run(fmt.Sprintf("history=%d", history), func(b *testing.B) {
			store, project, issue := incrementalTestStore(b)
			ctx := b.Context()
			job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Actor: "tester", Name: "History", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 3600}, Action: cron.Action{Kind: "execute", Prompt: "Review service"}, Issue: &cron.IssuePolicy{Kind: "existing", UID: issue.UID}, Overlap: "allow", Catchup: "latest"}})
			require.NoError(b, err)
			tx, err := store.BeginTx(ctx, nil)
			require.NoError(b, err)
			defer func() { _ = tx.Rollback() }()
			writer := store.cronSQL()
			writer.Transact = func(_ context.Context, write func(*sql.Tx) error) error { return write(tx) }
			for range history {
				id, err := uid.New()
				require.NoError(b, err)
				_, err = writer.ObserveRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "tester", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: cron.Summary{Version: 1, Message: strings.Repeat("x", 1024), InputTokens: 100, OutputTokens: 20}})
				require.NoError(b, err)
			}
			require.NoError(b, tx.Commit())
			b.Run("requirements", func(b *testing.B) {
				for b.Loop() {
					features, err := db.ProjectRequiredEventFeatures(ctx, store, project.UID)
					require.NoError(b, err)
					require.Equal(b, db.CronEventFeature, features)
				}
			})
			b.Run("exists", func(b *testing.B) {
				for b.Loop() {
					var present, moved bool
					err := store.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects p WHERE p.uid=$1 AND (
 EXISTS(SELECT 1 FROM cron_jobs WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM cron_workflows WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM cron_runs WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM events WHERE project_id=p.id AND type LIKE 'cron.%'))),
 EXISTS(SELECT 1 FROM projects p WHERE p.uid=$1 AND p.name<>$2 AND EXISTS(
 SELECT 1 FROM events e WHERE e.project_id=p.id AND e.type='issue.moved'))`, project.UID, db.SystemProjectName).Scan(&present, &moved)
					require.NoError(b, err)
					require.True(b, present)
					require.False(b, moved)
				}
			})
			b.Run("reason_miss", func(b *testing.B) {
				for b.Loop() {
					runs, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, Reason: "timeout", Limit: 100})
					require.NoError(b, err)
					require.Empty(b, runs)
				}
			})
		})
	}
}

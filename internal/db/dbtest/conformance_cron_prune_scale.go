package dbtest

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Reproduces the confirmed first-batch deadline with fleet-sized history.
// Explicit opt-in keeps the multi-GB fixture out of ordinary conformance runs.
func checkCronPruneHistoryScale(t *testing.T, s db.Storage, backend Backend) error {
	if os.Getenv("KATA_TEST_PRUNE_SCALE") != "1" {
		t.Skip("set KATA_TEST_PRUNE_SCALE=1 for 1.5 million runs / 3 million events")
	}
	p, q := pruneEventFixture(t, s)
	ctx := t.Context()
	// Twenty-five independent job streams, with two observations per run and
	// ordinary 1 KiB summaries, match the investigated fleet workload.
	for i := 1; i < 25; i++ {
		_, err := q.ExecContext(ctx, `INSERT INTO cron_jobs(uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,created_at,updated_at)
 SELECT $1,project_id,$2,definition_json,definition_event_uid,definition_hlc_json,author,created_at,updated_at FROM cron_jobs WHERE project_id=$3 AND uid=$4`, fmt.Sprintf("%026d", 100+i), fmt.Sprintf("Job %d", i), p.ID, fmt.Sprintf("%026d", 100))
		require.NoError(t, err)
	}
	pad := func(expr string) string { return `printf('%026d',` + expr + `)` }
	if backend.Name == "postgres" {
		pad = func(expr string) string { return `lpad((` + expr + `)::text,26,'0')` }
	}
	job, err := s.CronJob(ctx, p.ID, fmt.Sprintf("%026d", 100))
	require.NoError(t, err)
	summary := cron.Summary{Version: 1, Message: strings.Repeat("s", 1024)}
	summaryJSON, err := json.Marshal(summary)
	require.NoError(t, err)
	jobMarker := "job-marker"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const runs = 1500000
	for start := 1; start <= runs; start += 25000 {
		end := min(start+24999, runs)
		_, err := q.ExecContext(ctx, fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT %d UNION ALL SELECT i+1 FROM n WHERE i<%d)
 INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,actor,status,summary_json,created_at,updated_at)
 SELECT %s,$1,%s,$2,'operator','succeeded',$3,'2026-01-01T00:00:00.000000000Z','2026-01-01T00:00:00Z' FROM n`, start, end, pad("i+1000"), pad("100+i%25")), p.ID, job.DefinitionEventUID, string(summaryJSON))
		require.NoError(t, err)
		for revision := 1; revision <= 2; revision++ {
			payload, err := json.Marshal(db.CronRunObservation{ProjectUID: p.UID, UID: "run-marker", JobUID: &jobMarker, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: "succeeded", Summary: summary, Revision: int64(revision), CreatedAt: at, UpdatedAt: at})
			require.NoError(t, err)
			_, err = q.ExecContext(ctx, fmt.Sprintf(`INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT %s,$1,$2,$3,'cron.run.observed','operator',replace(replace($4,'run-marker',uid),'job-marker',job_uid),1,0,$5
 FROM cron_runs WHERE project_id=$2 AND uid>=$6 AND uid<=$7`, pad(fmt.Sprintf("id*2+%d+10000000", revision))), s.InstanceUID(), p.ID, p.Name, string(payload), strings.Repeat("c", 64), fmt.Sprintf("%026d", start+1000), fmt.Sprintf("%026d", end+1000))
			require.NoError(t, err)
		}
	}
	count := func(t *testing.T, table string) int {
		t.Helper()
		var n int
		require.NoError(t, q.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE project_id=$1`, p.ID).Scan(&n))
		return n
	}
	wantEvents := count(t, "events")
	project, err := s.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	for _, call := range []struct {
		name  string
		limit int
		dry   bool
	}{{"preview_one", 1, true}, {"preview_batch", 5000, true}, {"first_delete", 5000, false}} {
		t.Run(call.name, func(t *testing.T) {
			started := time.Now()
			result, err := s.PruneCronRuns(t.Context(), db.PruneCronRuns{ProjectID: p.ID, Actor: "operator", Before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Keep: 10, Limit: call.limit, DryRun: call.dry})
			elapsed := time.Since(started)
			t.Logf("runs=%d events=%d elapsed=%s", runs, runs*2, elapsed)
			if err != nil || call.dry {
				require.Equal(t, runs, count(t, "cron_runs"))
				require.Equal(t, wantEvents, count(t, "events"))
				current, readErr := s.ProjectByID(ctx, p.ID)
				require.NoError(t, readErr)
				require.Equal(t, project.Metadata, current.Metadata)
				require.Equal(t, project.Revision, current.Revision)
			}
			require.NoError(t, err)
			require.Less(t, elapsed, 30*time.Second, "the measured fixture must fit the production transaction budget")
			require.Equal(t, call.limit, result.Pruned)
			require.Equal(t, call.limit*2, result.EventsDeleted)
			require.True(t, result.More)
			if !call.dry {
				require.Equal(t, runs-call.limit, count(t, "cron_runs"))
				require.Equal(t, wantEvents-call.limit*2+1, count(t, "events"))
				counts, err := s.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: p.ID})
				require.NoError(t, err)
				var total int64
				for _, group := range counts.Groups {
					total += group.PrunedSinceTracking.Succeeded
				}
				require.Equal(t, int64(call.limit), total)
			}
		})
	}
	return nil
}

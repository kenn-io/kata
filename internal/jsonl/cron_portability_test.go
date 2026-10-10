package jsonl_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestNativeCronPostgresSequenceFloors(t *testing.T) {
	fixture := buildNativeCronJSONLFixture(t)
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	source, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(fixture.data), source))
	// Purged or rolled-back allocations may leave sequence floors above all
	// surviving rows. The portable backup must retain that allocation history.
	for _, table := range []string{"cron_jobs", "cron_workflows", "cron_runs"} {
		_, err = source.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence($1,'id'),700,true)`, table)
		require.NoError(t, err)
	}
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	target := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup.Bytes()), target))
	job := db.CronJob(*fixture.records[0].(*db.CronJobExport))
	workflow := db.CronWorkflow(*fixture.records[1].(*db.CronWorkflowExport))
	nextJob, _, err := target.PutCronJob(ctx, db.PutCronJob{ProjectID: fixture.projectID, Name: "Next job", Actor: "worker", Definition: job.Definition})
	require.NoError(t, err)
	assert.Greater(t, nextJob.ID, int64(700), "job floor survives PG to SQLite")
	nextWorkflow, _, err := target.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: fixture.projectID, Name: "Next workflow", Actor: "worker", Definition: workflow.Definition})
	require.NoError(t, err)
	assert.Greater(t, nextWorkflow.ID, int64(700), "workflow floor survives PG to SQLite")
	var runID int64
	err = target.QueryRowContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,occurrence_key,actor,status,summary_json)
 SELECT '01ARZ3NDEKTSV4RRFFQ69G5FAA',project_id,job_uid,definition_event_uid,occurrence_key,actor,status,summary_json FROM cron_runs LIMIT 1 RETURNING id`).Scan(&runID)
	require.NoError(t, err)
	assert.Greater(t, runID, int64(700), "run floor survives PG to SQLite")
}

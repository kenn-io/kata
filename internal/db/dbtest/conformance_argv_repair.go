package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A retained original can expose a projection normalized by an older release.
func checkArgvProjectionRepair(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, store, project)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	var payload string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT payload FROM events WHERE uid=$1`, workflow.DefinitionEventUID).Scan(&payload))
	var body db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal([]byte(payload), &body))
	body.Definition = []byte(`{"version":1,"steps":[null]}`)
	held, err := json.Marshal(body)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, string(held), workflow.DefinitionEventUID)
	require.NoError(t, err)
	id, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: id, WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.ErrorIs(t, err, cron.ErrInvalid, "retained held original must override normalized projection")
	var origin string
	var physical int64
	var counter int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT origin_instance_uid,hlc_physical_ms,hlc_counter FROM events WHERE uid=$1`, workflow.DefinitionEventUID).Scan(&origin, &physical, &counter))
	projection := db.FoldEvents([]db.FoldEvent{{UID: workflow.DefinitionEventUID, ProjectUID: project.UID, OriginInstanceUID: origin, HLCPhysicalMS: physical, HLCCounter: counter, Type: "cron.workflow.created", Payload: held}})
	materialize := func() {
		tx, err := q.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, db.MaterializeCronDefinitions(ctx, tx, project.ID, project.UID, projection, db.NewCronReplayValidator(tx, backend.Name == "postgres")))
		require.NoError(t, tx.Commit())
	}
	var raw string
	var revision int64
	materialize()
	require.NoError(t, q.QueryRowContext(ctx, `SELECT definition_json,revision FROM cron_workflows WHERE uid=$1`, workflow.UID).Scan(&raw, &revision))
	require.JSONEq(t, string(body.Definition), raw)
	materialize()
	var repeatRevision int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT revision FROM cron_workflows WHERE uid=$1`, workflow.UID).Scan(&repeatRevision))
	require.Equal(t, revision, repeatRevision)
	// A retained older held event must not replace a newer healthy row.
	newerClock := workflow.DefinitionHLC
	newerClock.PhysicalMS++
	clockJSON, err := json.Marshal(newerClock)
	require.NoError(t, err)
	newerEvent, err := uid.New()
	require.NoError(t, err)
	healthy, err := json.Marshal(workflow.Definition)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1,definition_hlc_json=$2,definition_event_uid=$3 WHERE uid=$4`, string(healthy), string(clockJSON), newerEvent, workflow.UID)
	require.NoError(t, err)
	materialize()
	require.NoError(t, q.QueryRowContext(ctx, `SELECT definition_json,revision FROM cron_workflows WHERE uid=$1`, workflow.UID).Scan(&raw, &repeatRevision))
	require.JSONEq(t, string(healthy), raw)
	require.Equal(t, revision, repeatRevision)
	// Keep identity evidence for a compacted job while its current revision advances.
	priorID, err := uid.New()
	require.NoError(t, err)
	prior := db.ObserveCronRun{ProjectID: project.ID, UID: priorID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1}}
	_, err = store.ObserveCronRun(ctx, prior)
	require.NoError(t, err)
	currentJob := job.Definition
	currentJob.Action.WorkflowUID = ""
	currentJob.Action.Prompt = "Review"
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: "Updated", Actor: "worker", Definition: currentJob})
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, job.DefinitionEventUID)
	require.NoError(t, err)
	fresh := prior
	fresh.UID, err = uid.New()
	require.NoError(t, err)
	fresh.Status = "running"
	_, err = store.ObserveCronRun(ctx, fresh)
	require.ErrorIs(t, err, cron.ErrInvalid, "compacted job identity does not establish its workflow")
	fresh.Status = "succeeded"
	_, err = store.ObserveCronRun(ctx, fresh)
	require.NoError(t, err)
	return nil
}

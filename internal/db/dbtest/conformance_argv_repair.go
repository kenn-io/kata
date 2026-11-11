package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
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
	// An equal-revision null kind must also repair a normalized row.
	body.Definition = []byte(`{"version":1,"steps":[{"kind":null}]}`)
	held, err = json.Marshal(body)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, string(held), workflow.DefinitionEventUID)
	require.NoError(t, err)
	healthyOriginal, err := json.Marshal(workflow.Definition)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1 WHERE uid=$2`, string(healthyOriginal), workflow.UID)
	require.NoError(t, err)
	projection = db.FoldEvents([]db.FoldEvent{{UID: workflow.DefinitionEventUID, ProjectUID: project.UID, OriginInstanceUID: origin, HLCPhysicalMS: physical, HLCCounter: counter, Type: "cron.workflow.created", Payload: held}})
	materialize()
	require.NoError(t, q.QueryRowContext(ctx, `SELECT definition_json,revision FROM cron_workflows WHERE uid=$1`, workflow.UID).Scan(&raw, &revision))
	require.JSONEq(t, string(body.Definition), raw)
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

// Full owner restore must choose a supported winner after an intermediate held edit.
func checkArgvOwnerRestoreSupportedWinner(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "first", Kind: "command", Command: "true"}}}
	original, _, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Original", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	oldRecords, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	var oldRow *db.CronWorkflowExport
	for _, record := range oldRecords {
		if row, ok := record.(*db.CronWorkflowExport); ok && row.UID == original.UID {
			oldRow = row
		}
	}
	require.NotNil(t, oldRow)
	heldEdit, _, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: original.UID, ExpectedEventUID: original.DefinitionEventUID, Name: "Intermediate", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	currentEvent := heldEdit.DefinitionEventUID
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "deletion"}[deleted], func(t *testing.T) {
			winner, _, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: original.UID, ExpectedEventUID: currentEvent, Name: "Winner", Actor: "worker", Definition: definition, Deleted: deleted})
			require.NoError(t, err)
			currentEvent = winner.DefinitionEventUID
			records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
			require.NoError(t, err)
			for i, record := range records {
				switch value := record.(type) {
				case *db.CronWorkflowExport:
					if value.UID == original.UID {
						records[i] = oldRow
					}
				case *db.EventExport:
					if value.UID == heldEdit.DefinitionEventUID {
						copyEvent := *value
						var body db.CronDefinitionEvent
						require.NoError(t, json.Unmarshal(value.Payload, &body))
						body.Definition = []byte(`{"version":1,"steps":[{"kind":"argv"}]}`)
						copyEvent.Payload, err = json.Marshal(body)
						require.NoError(t, err)
						records[i] = &copyEvent
					}
				}
			}
			target := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{RecomputeEventContentHash: true}))
			restoredProject, err := target.ProjectByUID(ctx, project.UID)
			require.NoError(t, err)
			got, err := target.CronWorkflow(ctx, restoredProject.ID, original.UID)
			require.NoError(t, err)
			require.Equal(t, winner.DefinitionEventUID, got.DefinitionEventUID)
			require.Equal(t, "Winner", got.Name)
			require.Equal(t, deleted, got.DeletedAt != nil)
		})
	}
	return nil
}

// Job-only running evidence and new jobs must use the effective raw workflow,
// including when an older projection normalized an unsupported retained event.
func checkArgvJobWorkflowAdmission(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, source, project)
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	for _, state := range []string{"missing after restore", "held projection", "retained raw"} {
		t.Run(state, func(t *testing.T) {
			fixture := append([]db.ImportRecord(nil), records...)
			if state == "missing after restore" {
				for i, record := range fixture {
					if event, ok := record.(*db.EventExport); ok && event.UID == workflow.DefinitionEventUID {
						copyEvent := *event
						var body db.CronDefinitionEvent
						require.NoError(t, json.Unmarshal(event.Payload, &body))
						body.Definition = []byte(`null`)
						copyEvent.Payload, err = json.Marshal(body)
						require.NoError(t, err)
						fixture[i] = &copyEvent
					}
				}
			}
			target := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			require.NoError(t, target.ImportReplay(ctx, fixture, db.ImportOptions{RecomputeEventContentHash: true}))
			p, err := target.ProjectByUID(ctx, project.UID)
			require.NoError(t, err)
			q := target.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
				QueryRowContext(context.Context, string, ...any) *sql.Row
			})
			if state == "missing after restore" {
				var count int
				require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_workflows WHERE uid=$1`, workflow.UID).Scan(&count))
				require.Zero(t, count, "unrepresentable retained workflow has no projection")
			}
			if state == "held projection" {
				_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1 WHERE uid=$2`, `{"version":1,"steps":[null]}`, workflow.UID)
				require.NoError(t, err)
			}
			if state == "retained raw" {
				var payload string
				require.NoError(t, q.QueryRowContext(ctx, `SELECT payload FROM events WHERE uid=$1`, workflow.DefinitionEventUID).Scan(&payload))
				var body db.CronDefinitionEvent
				require.NoError(t, json.Unmarshal([]byte(payload), &body))
				body.Definition = []byte(`{"version":1,"steps":[null]}`)
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				_, err = q.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, string(raw), workflow.DefinitionEventUID)
				require.NoError(t, err)
				_, _, err = target.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "New job", Actor: "worker", Definition: job.Definition})
				assert.ErrorIs(t, err, cron.ErrInvalid, "new jobs must reject the retained unsupported workflow")
			}
			runUID, err := uid.New()
			require.NoError(t, err)
			_, err = target.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: p.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
			require.ErrorIs(t, err, cron.ErrInvalid, "new job-only running evidence must reject %s workflow", state)
		})
	}
	return nil
}

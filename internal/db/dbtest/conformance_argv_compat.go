package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Unsupported imported rows fail on reads, lists and snapshot construction,
// including tombstones. Test-local SQL does not define production state.
func checkArgvCompatStoredReads(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Legacy", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "first", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	for _, deleted := range []any{nil, "2026-11-08T12:00:00Z"} {
		_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1,deleted_at=$2 WHERE uid=$3`, `{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`, deleted, workflow.UID)
		require.NoError(t, err)
		_, err = store.CronWorkflow(ctx, project.ID, workflow.UID)
		require.Error(t, err)
		_, err = store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID, IncludeDeleted: true})
		require.Error(t, err)
		tx, err := q.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, snapshotErr := db.CronDefinitionSnapshots(ctx, tx, project)
		require.NoError(t, tx.Rollback())
		require.Error(t, snapshotErr)
	}
	return nil
}

func argvCompatRemoteWorkflow(t *testing.T, project db.Project, origin, definition string) db.RemoteEvent {
	t.Helper()
	workflowUID, err := uid.New()
	require.NoError(t, err)
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(db.CronDefinitionEvent{UID: workflowUID, ProjectUID: project.UID, Name: "Example workflow", Definition: jsontext.Value(definition), Author: "worker", CreatedAt: at, UpdatedAt: at})
	require.NoError(t, err)
	return newRemoteEvent(t, project, nil, "cron.workflow.created", "worker", origin, at.UnixMilli(), payload)
}

// A reserved kind cannot enter through decode-only historical acceptance or
// preserve a valid prefix of a refused batch, even with a claimed capability.
func checkArgvCompatRemoteRollback(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	good := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"key":"legacy","kind":"command","command":"true"}]}`)
	reserved := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`)
	binding, err := store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	binding.PullCursorEventID, binding.PushCursorEventID = 17, 23
	binding, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	requireCursors := func() {
		current, err := store.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, binding.PullCursorEventID, current.PullCursorEventID)
		require.Equal(t, binding.PushCursorEventID, current.PushCursorEventID)
	}
	requireAbsent := func() {
		for _, eventUID := range []string{good.EventUID, reserved.EventUID} {
			_, err := store.EventsByUIDs(ctx, project.ID, []string{eventUID})
			require.ErrorIs(t, err, db.ErrNotFound)
		}
	}
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{good, reserved})
	require.Error(t, err)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	requireAbsent()
	requireCursors()

	before, err = store.MaxEventID(ctx)
	require.NoError(t, err)
	params := db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures + ",cron_workflow_argv_v1", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: good}, {SourceEventID: 2, Event: reserved}}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.Error(t, err)
	after, err = store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	requireAbsent()
	requireCursors()

	malformed := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"kind":null}]}`)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: malformed}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	_, err = store.EventsByUIDs(ctx, project.ID, []string{malformed.EventUID})
	require.ErrorIs(t, err, db.ErrNotFound)
	requireCursors()
	return nil
}

package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

// A binding that changes as the transaction acquires its project write lock
// must invalidate the earlier standalone admission before any evidence is saved.
func TestCronSummaryBindingFenceAfterProjectLock(t *testing.T) {
	tx, _, event := referenceWorkFixture(t, 0)
	for _, statement := range []string{
		`CREATE TABLE federation_bindings(project_id INTEGER,role TEXT,hub_url TEXT,hub_project_id INTEGER,hub_project_uid TEXT,push_enabled BOOLEAN,bound_actor TEXT,allow_insecure BOOLEAN,enabled BOOLEAN,relay_config TEXT)`,
		`CREATE TRIGGER concurrent_join AFTER UPDATE ON projects BEGIN INSERT INTO federation_bindings VALUES(1,'spoke','https://daemon.example',1,'01M40000000000000000000003',1,'worker',0,1,NULL); END`,
	} {
		_, err := tx.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	var in CronRunObservation
	require.NoError(t, json.Unmarshal(event.Payload, &in))
	sqlCron := CronSQL{
		Transact:    func(_ context.Context, fn func(*sql.Tx) error) error { return fn(tx) },
		WriteGate:   func(context.Context, *sql.Tx, int64) error { return nil },
		InsertEvent: func(context.Context, *sql.Tx, CronEvent) (Event, error) { return Event{UID: event.EventUID}, nil },
	}
	_, err := sqlCron.ObserveRun(WithCronSummaryBinding(t.Context(), nil), ObserveCronRun{ProjectID: 1, UID: in.UID, JobUID: in.JobUID, DefinitionEventUID: in.DefinitionEventUID, WorkflowUID: in.WorkflowUID, WorkflowDefinitionEventUID: in.WorkflowDefinitionEventUID, Actor: in.Actor, Status: in.Status, Summary: cron.Summary{Version: 1, Reason: "failed"}})
	require.ErrorIs(t, err, ErrUnsupportedEventFeatures)
	var runs int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM cron_runs`).Scan(&runs))
	require.Zero(t, runs)
}

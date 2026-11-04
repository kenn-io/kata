package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// CheckCronValidateSnapshot coordinates a dependency change after the read
// snapshot is established. The writer must commit while that snapshot is open,
// and validation must use the dependency revision from the same snapshot.
func CheckCronValidateSnapshot(t *testing.T, reader db.CronSQL, writer db.Storage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	project, err := writer.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	workflow := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "review", Kind: "prompt", Prompt: "Review"}}}
	stored, _, err := writer.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Review", Actor: "operator", Definition: workflow})
	require.NoError(t, err)
	job := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", WorkflowUID: stored.UID}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "latest"}
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	in := db.CronValidationInput{ProjectID: project.ID, Name: "Review", Raw: raw, From: time.Now().UTC(), Count: 5}
	readTransaction := reader.ReadTransact
	reader.ReadTransact = func(ctx context.Context, run func(*sql.Tx) error) error {
		return readTransaction(ctx, func(tx *sql.Tx) error {
			var id int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE id=$1`, project.ID).Scan(&id); err != nil {
				return err
			}
			_, _, err := writer.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: stored.UID, ExpectedEventUID: stored.DefinitionEventUID, Name: stored.Name, Definition: workflow, Actor: "operator", Deleted: true})
			if err != nil {
				return err
			}
			return run(tx)
		})
	}
	out, err := reader.ValidateJob(ctx, in)
	require.NoError(t, err)
	require.True(t, out.Valid, "%+v", out.Errors)
	out, err = writer.ValidateCronJob(ctx, in)
	require.NoError(t, err)
	require.False(t, out.Valid)
	require.Equal(t, "dependencies", out.Errors[0].Stage)
}

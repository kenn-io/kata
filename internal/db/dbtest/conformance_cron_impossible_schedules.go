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
)

// Impossible writes leave definitions untouched; stale rows remain readable and
// deletable, and a failed sweep cannot create children or advance progress.
func checkCronRejectsImpossibleSchedules(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, parent, job, progress := recurringFixture(t, store, "all")
	t.Run("create", func(t *testing.T) {
		before, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
		require.NoError(t, err)
		definition := job.Definition
		definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
		_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Impossible review", Definition: definition, Actor: "operator"})
		require.ErrorIs(t, err, cron.ErrInvalid)
		require.ErrorContains(t, err, "never fires")
		after, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
		require.NoError(t, err)
		require.Equal(t, before, after)
	})
	t.Run("update", func(t *testing.T) {
		definition := job.Definition
		definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 31 4 *"}
		_, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, Definition: definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
		require.ErrorIs(t, err, cron.ErrInvalid)
		require.ErrorContains(t, err, "never fires")
		after, err := store.CronJob(ctx, project.ID, job.UID)
		require.NoError(t, err)
		require.Equal(t, job, after)
	})
	t.Run("stored sweep and delete", func(t *testing.T) {
		stored, err := store.CronJob(ctx, project.ID, job.UID)
		require.NoError(t, err)
		stored.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 0 30 2 *"}
		raw, err := json.Marshal(stored.Definition)
		require.NoError(t, err)
		q := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), job.UID)
		require.NoError(t, err)
		before, err := store.IssueByID(ctx, parent.ID)
		require.NoError(t, err)
		var beforeMeta map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(before.Metadata), &beforeMeta))
		key := "cron.progress." + job.UID
		require.NotEmpty(t, beforeMeta[key])
		issuesBefore, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
		require.NoError(t, err)
		_, err = store.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: project.ID, JobUID: job.UID, ExpectedEventUID: stored.DefinitionEventUID, Now: progress.Anchor.Add(24 * time.Hour)})
		require.ErrorIs(t, err, cron.ErrInvalid)
		require.ErrorContains(t, err, "never fires")
		after, err := store.IssueByID(ctx, parent.ID)
		require.NoError(t, err)
		var afterMeta map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(after.Metadata), &afterMeta))
		require.Equal(t, beforeMeta[key], afterMeta[key])
		issuesAfter, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
		require.NoError(t, err)
		require.Equal(t, issuesBefore, issuesAfter)
		deleted, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: stored.Name, Definition: stored.Definition, ExpectedEventUID: stored.DefinitionEventUID, Actor: "operator", Deleted: true})
		require.NoError(t, err)
		require.NotNil(t, deleted.DeletedAt)
	})
	return nil
}

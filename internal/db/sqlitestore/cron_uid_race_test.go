package sqlitestore_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A writer in another project can claim a fresh cron UID after this write's
// existence check and before its insert. A test-local trigger stands in for
// that writer so the insert, not the earlier check, meets the collision.
func TestCronFreshUIDCollisionIsAConflict(t *testing.T) {
	ctx := t.Context()
	store := openTestDB(t)
	writer, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	rival, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: writer.ID, Name: "Review", Definition: definition, Actor: "worker"})
	require.NoError(t, err)

	t.Run("definition", func(t *testing.T) {
		_, err := store.ExecContext(ctx, fmt.Sprintf(`CREATE TEMP TRIGGER claim_job BEFORE INSERT ON cron_jobs WHEN NEW.project_id = %d BEGIN
  INSERT INTO cron_jobs(uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at)
  VALUES(NEW.uid,%d,NEW.name,NEW.definition_json,NEW.definition_event_uid,NEW.definition_hlc_json,'rival',1,NEW.created_at,NEW.updated_at);
END`, writer.ID, rival.ID))
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = store.ExecContext(ctx, `DROP TRIGGER IF EXISTS claim_job`) })
		before, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: writer.ID, Name: "Racing", Definition: definition, Actor: "worker"})
		require.ErrorIs(t, err, db.ErrCronConflict)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after, "the losing write commits no event")
	})

	t.Run("run", func(t *testing.T) {
		_, err := store.ExecContext(ctx, fmt.Sprintf(`CREATE TEMP TRIGGER claim_run BEFORE INSERT ON cron_runs WHEN NEW.project_id = %d BEGIN
  INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,actor,status,created_at,updated_at)
  VALUES(NEW.uid,%d,NEW.job_uid,NEW.definition_event_uid,'rival','running',NEW.created_at,NEW.updated_at);
END`, writer.ID, rival.ID))
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = store.ExecContext(ctx, `DROP TRIGGER IF EXISTS claim_run`) })
		before, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		runUID, err := uid.New()
		require.NoError(t, err)
		_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: writer.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
		require.ErrorIs(t, err, db.ErrCronConflict)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after, "the losing write commits no event")
		_, err = store.CronRun(ctx, rival.ID, runUID)
		require.ErrorIs(t, err, db.ErrNotFound, "the rolled-back transaction leaves no row behind")
	})
}

package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCronDataBlocksOrphanCleanup(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	definition := cron.JobDefinition{
		Version: 1,
		Kind:    "job",
		Trigger: cron.Trigger{Kind: "manual"},
		Action:  cron.Action{Kind: "execute", Prompt: "Review"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Review"},
		Overlap: "forbid",
		Catchup: "skip",
	}
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	occurrence := "manual:review"
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{
		ProjectID: project.ID, UID: runUID, JobUID: &job.UID,
		DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence,
		Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1},
	})
	require.NoError(t, err)

	_, err = store.HardDeleteProject(ctx, project.ID)
	require.Error(t, err, "orphan cleanup must retain accepted cron data and let project FK protection refuse deletion")
	gotProject, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, project.UID, gotProject.UID)
	gotJob, err := store.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, job.UID, gotJob.UID)
	gotRun, err := store.CronRun(ctx, project.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, runUID, gotRun.UID)
	return nil
}

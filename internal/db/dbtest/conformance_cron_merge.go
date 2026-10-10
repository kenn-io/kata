package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCronProjectMergeRestore(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	sourceProject, err := source.CreateProject(ctx, "merge-source")
	require.NoError(t, err)
	targetProject, err := source.CreateProject(ctx, "merge-target")
	require.NoError(t, err)

	job, workflow := nativeFederationDefinitions(t, source, sourceProject)
	runUID, err := uid.New()
	require.NoError(t, err)
	_, err = source.ObserveCronRun(ctx, db.ObserveCronRun{
		ProjectID: sourceProject.ID, UID: runUID, JobUID: &job.UID,
		DefinitionEventUID: &job.DefinitionEventUID, WorkflowUID: &workflow.UID,
		WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker",
		Status: "running", Summary: cron.Summary{Version: 1},
	})
	require.NoError(t, err)

	_, err = source.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: sourceProject.ID, TargetProjectID: targetProject.ID, Actor: "worker",
	})
	require.NoError(t, err)
	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)

	var cronEventUIDs []string
	for _, record := range records {
		event, ok := record.(*db.EventExport)
		if ok && strings.HasPrefix(event.Type, "cron.") {
			cronEventUIDs = append(cronEventUIDs, event.UID)
		}
	}
	require.NotEmpty(t, cronEventUIDs)
	mergedEvents, err := source.EventsByUIDs(ctx, targetProject.ID, cronEventUIDs)
	require.NoError(t, err)
	require.Len(t, mergedEvents, len(cronEventUIDs))
	for _, event := range mergedEvents {
		_, _, err := db.ValidateRemoteEventContentHash(portableNativeEvent(event))
		require.NoError(t, err, "merged cron events retain valid portable content hashes")
	}

	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	_, err = restored.CreateProject(ctx, "retained-project")
	require.NoError(t, err)
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}), "a merged cron history export must restore")

	target, err := restored.ProjectByUID(ctx, targetProject.UID)
	require.NoError(t, err)
	restoredJob, err := restored.CronJob(ctx, target.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, job.UID, restoredJob.UID)
	restoredWorkflow, err := restored.CronWorkflow(ctx, target.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, workflow.UID, restoredWorkflow.UID)
	restoredRun, err := restored.CronRun(ctx, target.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, runUID, restoredRun.UID)

	return nil
}

package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func checkFederationRejoinCronLocalEvents(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "rejoin-cron")
	require.NoError(t, err)
	workflowDefinition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}
	workflow, workflowEvent, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "workflow", Actor: "old-actor", Definition: workflowDefinition})
	require.NoError(t, err)
	jobDefinition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", WorkflowUID: workflow.UID}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, jobEvents, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "job", Actor: "old-actor", Definition: jobDefinition})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	_, workflowEdit, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: workflow.UID, ExpectedEventUID: workflow.DefinitionEventUID, Name: "updated workflow", Actor: "new-actor", Definition: workflowDefinition})
	require.NoError(t, err)
	_, jobEdit, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: "updated job", Actor: "new-actor", Definition: jobDefinition})
	require.NoError(t, err)
	kept, err := store.KeepFederationDependentLocal(ctx, project.ID, workflowEdit.ID)
	require.NoError(t, err)
	require.False(t, kept)
	kept, err = store.FederationKeptDependent(ctx, project.ID, workflowEdit.ID)
	require.NoError(t, err)
	require.False(t, kept)
	for _, event := range []db.Event{workflowEvent, jobEvents[0]} {
		kept, err := store.KeepFederationEventLocal(ctx, project.ID, event.ID)
		require.NoError(t, err)
		require.True(t, kept)
	}
	for _, event := range []db.Event{workflowEdit, jobEdit[0]} {
		known, err := store.FederationKeptDependent(ctx, project.ID, event.ID)
		require.NoError(t, err)
		require.True(t, known)
		kept, err := store.KeepFederationDependentLocal(ctx, project.ID, event.ID)
		require.NoError(t, err)
		require.True(t, kept)
	}
	return nil
}

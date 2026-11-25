package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederationRejoinLocalEvents(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "rejoin-project")
	require.NoError(t, err)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373", HubProjectID: 1, HubProjectUID: project.UID, Actor: "old-actor", Enabled: true, PushEnabled: true}
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	old, retained, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained", Author: "old-actor"})
	require.NoError(t, err)
	eligibleIssue, eligible, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "eligible", Author: "new-actor"})
	require.NoError(t, err)
	shared, sharedEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "shared task", Author: "old-actor"})
	require.NoError(t, err)
	binding.Actor = "new-actor"
	binding.PushEnabled = false
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	binding, err = store.EnableFederationPush(ctx, project.ID, cursor)
	require.NoError(t, err)
	_, comment, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local"})
	require.NoError(t, err)
	_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later", Author: "new-actor"})
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "rejoin-target")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: binding.HubURL, HubProjectID: 2, HubProjectUID: target.UID, Actor: binding.Actor, Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	currentOld, err := store.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	for _, preview := range []bool{true, false} {
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: old.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: currentOld.Revision, Actor: "new-actor", DryRun: preview})
		require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
	}
	currentEligible, err := store.IssueByUID(ctx, eligibleIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: currentEligible.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: currentEligible.Revision, Actor: "new-actor", DryRun: true})
	require.NoError(t, err)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, eligible.ID)
	require.NoError(t, err)
	require.False(t, kept)
	kept, err = store.KeepFederationEventLocal(ctx, project.ID, retained.ID)
	require.NoError(t, err)
	require.True(t, kept)
	kept, err = store.FederationKeptIssue(ctx, project.ID, old.UID)
	require.NoError(t, err)
	require.True(t, kept)
	kept, err = store.FederationKeptIssue(ctx, project.ID, eligibleIssue.UID)
	require.NoError(t, err)
	require.False(t, kept)
	binding, err = store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, retained.ID, binding.PushCursorEventID)
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Contains(t, pending, comment, "an actor refusal alone does not prove the hub lacks the task")
	kept, err = store.KeepFederationDependentLocal(ctx, project.ID, comment.ID)
	require.NoError(t, err)
	require.True(t, kept)
	kept, err = store.KeepFederationEventLocal(ctx, project.ID, sharedEvent.ID)
	require.NoError(t, err)
	require.True(t, kept)
	currentOld, err = store.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	for _, preview := range []bool{true, false} {
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: old.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: currentOld.Revision, Actor: "new-actor", DryRun: preview})
		require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
	}
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, later.ID))
	err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, store.InstanceUID(), later.ID)
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{retained.UID, comment.UID})
	require.NoError(t, err)
	wantEvents := []db.Event{retained, comment}
	require.Len(t, events, len(wantEvents))
	for i, event := range events {
		require.Equal(t, wantEvents[i].UID, event.UID)
		require.Equal(t, wantEvents[i].ContentHash, event.ContentHash)
	}
	currentOld, err = store.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	{
		paused, err := store.FederationBindingByProject(ctx, target.ID)
		require.NoError(t, err)
		paused.PushEnabled = false
		_, err = store.UpsertFederationBinding(ctx, paused)
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: currentOld.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: currentOld.Revision, Actor: "new-actor", DryRun: true})
		require.ErrorIs(t, err, db.ErrFederatedReadOnly)
		require.NotErrorIs(t, err, db.ErrFederatedMoveUnsupported)
		paused.PushEnabled = true
		_, err = store.UpsertFederationBinding(ctx, paused)
		require.NoError(t, err)
	}
	remoteUID, err := uid.New()
	require.NoError(t, err)
	remote := newRemoteEvent(t, project, &shared.UID, "issue.updated", "hub-actor", remoteUID, 500, jsontext.Value(`{"title":"shared task"}`))
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, remote)
	require.NoError(t, err)
	require.True(t, inserted)
	shared, err = store.IssueByUID(ctx, shared.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	for _, preview := range []bool{true, false} {
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: shared.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: shared.Revision, Actor: "new-actor", DryRun: preview})
		require.NoError(t, err)
	}
	targetPending, err := store.PendingFederationPushEvents(ctx, target.ID, store.InstanceUID(), 0, 100)
	require.NoError(t, err)
	require.NotEmpty(t, targetPending)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, targetPending[len(targetPending)-1].ID))
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	standalone, err := store.CreateProject(ctx, "standalone-target")
	require.NoError(t, err)
	currentOld, err = store.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: currentOld.ID, FromProjectID: project.ID, ToProjectID: standalone.ID, IfMatchRev: currentOld.Revision, Actor: "new-actor"})
	require.NoError(t, err)
	{
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
		for _, event := range []db.Event{workflowEvent, jobEvents[0]} {
			kept, err := store.KeepFederationEventLocal(ctx, project.ID, event.ID)
			require.NoError(t, err)
			require.True(t, kept)
		}
		for _, event := range []db.Event{workflowEdit, jobEdit[0]} {
			kept, err := store.KeepFederationDependentLocal(ctx, project.ID, event.ID)
			require.NoError(t, err)
			require.True(t, kept)
		}
	}
	return nil
}

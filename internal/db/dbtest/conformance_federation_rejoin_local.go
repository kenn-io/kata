package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
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
	_, localComment, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: eligibleIssue.ID, Author: "old-actor", Body: "local discussion"})
	require.NoError(t, err)
	binding.Actor = "new-actor"
	binding.PushEnabled = false
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	binding, err = store.EnableFederationPush(ctx, project.ID, 0)
	require.NoError(t, err)
	_, comment, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later", Author: "new-actor"})
	require.NoError(t, err)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, eligible.ID)
	require.NoError(t, err)
	require.False(t, kept)
	kept, err = store.KeepFederationEventLocal(ctx, project.ID, retained.ID)
	require.NoError(t, err)
	require.True(t, kept)
	kept, err = store.KeepFederationEventLocal(ctx, project.ID, localComment.ID)
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
	return nil
}

func checkFederationRejoinResetRetention(t *testing.T, store db.Storage) error {
	t.Run("cross-task reply", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "reset-reply")
		require.NoError(t, err)
		bindRejoinSpokes(t, store, project)
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		target, targetCreated, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "target", Author: "old-actor"})
		require.NoError(t, err)
		source, sourceCreated, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "source", Author: "old-actor"})
		require.NoError(t, err)
		comment, root, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "old-actor", Body: "retained"})
		require.NoError(t, err)
		bindRejoinSpokes(t, store, project)
		for _, event := range []db.Event{targetCreated, sourceCreated, root} {
			kept, err := store.KeepFederationEventLocal(ctx, project.ID, event.ID)
			require.NoError(t, err)
			require.True(t, kept)
		}
		_, reply, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "new-actor", Body: "reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
		require.NoError(t, err)
		kept, err := store.KeepFederationCommentDependentLocal(ctx, project.ID, reply.ID)
		require.NoError(t, err)
		require.True(t, kept)
		binding, err := store.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		require.NoError(t, store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, store.InstanceUID(), binding.PushCursorEventID))
		events, err := store.EventsByUIDs(ctx, project.ID, []string{root.UID, reply.UID})
		require.NoError(t, err)
		require.Len(t, events, 2)
		for i, want := range []db.Event{root, reply} {
			require.Equal(t, want.UID, events[i].UID)
			require.Equal(t, want.ContentHash, events[i].ContentHash)
		}
		target, err = store.IssueByUID(ctx, target.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		source, err = store.IssueByUID(ctx, source.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		targetComments, err := store.CommentsByIssue(ctx, target.ID)
		require.NoError(t, err)
		require.Len(t, targetComments, 1)
		sourceComments, err := store.CommentsByIssue(ctx, source.ID)
		require.NoError(t, err)
		require.Len(t, sourceComments, 1)
		require.Equal(t, "retained", targetComments[0].Body)
		require.Equal(t, "reply", sourceComments[0].Body)
		require.Equal(t, targetComments[0].UID, sourceComments[0].ReplyToUID)
	})
	t.Run("retained root", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "rejoin-reset")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		issue, retained, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained", Author: "old-actor"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
		kept, err := store.KeepFederationEventLocal(ctx, project.ID, retained.ID)
		require.NoError(t, err)
		require.True(t, kept)
		_, comment, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "retained discussion"})
		require.NoError(t, err)
		kept, err = store.KeepFederationDependentLocal(ctx, project.ID, comment.ID)
		require.NoError(t, err)
		require.True(t, kept)
		_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later", Author: "new-actor"})
		require.NoError(t, err)
		require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, later.ID))
		require.NoError(t, store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, store.InstanceUID(), later.ID))
		events, err := store.EventsByUIDs(ctx, project.ID, []string{retained.UID, comment.UID})
		require.NoError(t, err)
		require.Len(t, events, 2)
		for i, want := range []db.Event{retained, comment} {
			require.Equal(t, want.UID, events[i].UID)
			require.Equal(t, want.ContentHash, events[i].ContentHash)
		}
	})
	return nil
}

func checkFederationRejoinCronDependents(t *testing.T, store db.Storage) error {
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
	return nil
}

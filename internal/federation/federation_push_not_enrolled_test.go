package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A spoke comments on its copy of an issue that the hub has meanwhile moved
// to a project the spoke has not joined. The hub refuses the push. The pull
// must still run, so hub work keeps arriving and the move parks the issue
// instead of the spoke stalling on the refusal (#1049).
func TestSyncFederationPullsAfterPushRefusedForUnjoinedProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "joined-project", "absent-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moves away", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "offline comment"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	later, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "later hub work", Author: "worker"})
	require.NoError(t, err)

	err = spoke.sync(t, 0)
	require.ErrorContains(t, err, "federation_project_not_enrolled", "the refusal stays visible")
	require.ErrorContains(t, err, issue.UID)

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID, "the pull runs after the refused push")
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	parked, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotEqual(t, spoke.projects[0].ID, parked.ProjectID, "the pulled move parks the issue outside the joined project")
}

func TestSyncFederationHeldEditAfterBothProjectsReset(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "reset-source", "reset-destination", "reset-absent")
	spoke := newMoveDeliverySpoke(t, hub, projects[:2])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "held edit", Author: "worker"})
	require.NoError(t, err)
	for i := range spoke.projects {
		require.NoError(t, spoke.sync(t, i))
	}
	local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, comment, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "held comment"})
	require.NoError(t, err)
	away, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.ErrorContains(t, spoke.sync(t, 0), "federation_project_not_enrolled")
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: away.Issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, 0, 0))
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[1].ID)
	require.NoError(t, err)
	require.NoError(t, spoke.env.DB.ResetFederatedProjectIfNoPendingPush(ctx, spoke.projects[1].ID, 0, 0, spoke.env.DB.InstanceUID(), binding.PushCursorEventID))
	require.NoError(t, spoke.sync(t, 1))
	replayed, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[1].ID, replayed.ProjectID)
	require.NoError(t, spoke.sync(t, 0))
	settled, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[1].ID, settled.ProjectID)
	comments, err := hub.DB.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "held comment", comments[0].Body)
	events, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{comment.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, comment.ContentHash, events[0].ContentHash)
}

package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

// A hub edit and move into an unjoined project must not strand the spoke's issue.
func TestSyncFederationEditedIssueMovedToUnjoinedProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "source", "destination")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Issue", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	earlierIssue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Earlier local work", Author: "worker"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Unsynced local comment"})
	require.NoError(t, err)
	title := "Hub edit"
	edited, _, _, err := hub.DB.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "worker"})
	require.NoError(t, err)
	move, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: edited.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, later, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Later work", Author: "worker"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	var delivered []string
	err = SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[0].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
		for _, event := range events {
			if event.UID == move.Event.UID || event.UID == later.UID {
				delivered = append(delivered, event.UID)
			}
		}
	}, spoke.credentials)
	require.ErrorContains(t, err, "403", "the rejected push is still reported")
	require.Equal(t, []string{move.Event.UID, later.UID}, delivered)
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, later.ID)
	retained, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, local.ID, retained.ID)
	require.Equal(t, "Hub edit", retained.Title)
	events, err := spoke.env.DB.EventsByUIDs(ctx, retained.ProjectID, []string{move.Event.UID})
	require.NoError(t, err)
	require.Len(t, events, 1, "the hub's event UID is retained")
	require.Equal(t, move.Event.ContentHash, events[0].ContentHash)
	_, _, _, err = spoke.env.DB.RestoreProject(ctx, retained.ProjectID, "worker")
	require.NoError(t, err)
	other, err := spoke.env.DB.CreateProject(ctx, "other")
	require.NoError(t, err)
	_, err = spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: retained.ID, FromProjectID: retained.ProjectID, ToProjectID: other.ID, IfMatchRev: retained.Revision, Actor: "worker"})
	require.NoError(t, err)
	newIssue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Later local work", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	remote, err := hub.DB.IssueByUID(ctx, newIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Later local work", remote.Title)
	_, err = hub.DB.IssueByUID(ctx, earlierIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

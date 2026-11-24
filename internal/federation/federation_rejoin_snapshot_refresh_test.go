package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRejoinPublishesNewRetainedCommentAfterUnacceptedSnapshot(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "snapshot-refresh")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, _, err = spoke.env.DB.RetainedIssueSnapshot(ctx, spoke.projects[0].ID, issue.UID)
	require.NoError(t, err)
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "added after leaving"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "new edit"})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	require.Equal(t, "added after leaving", comments[0].Body)
	require.Equal(t, "new edit", comments[1].Body)
}

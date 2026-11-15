package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestSyncFederationMovedIssuePreservesPreEnrollmentAuthorRewrite(t *testing.T) {
	ctx := t.Context()
	hub := testenv.New(t)
	projects := make([]db.Project, 2)
	for i, name := range []string{"author-baseline-source", "author-baseline-target"} {
		var err error
		projects[i], err = hub.DB.CreateProject(ctx, name)
		require.NoError(t, err)
	}
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "rewritten before enrollment", Author: "original-author"})
	require.NoError(t, err)
	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "original-author", Body: "retained discussion"})
	require.NoError(t, err)
	_, err = hub.DB.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{ProjectID: projects[0].ID, Actor: "worker", From: "original-author", To: "renamed-author"})
	require.NoError(t, err)
	for _, project := range projects {
		_, err = hub.DB.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	spoke := newMoveDeliverySpoke(t, hub, projects)
	require.NoError(t, spoke.sync(t, 1), "cold destination sync backfills creation and its effective source baseline")
	got, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[1].ID, got.ProjectID)
	require.Equal(t, "renamed-author", got.Author)
	comments, err := spoke.env.DB.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "renamed-author", comments[0].Author)
}

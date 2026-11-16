package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A spoke joined to one project keeps pulling when an issue moves in from a
// project the spoke never joined (#1046).
func TestSyncFederationPullsMoveFromUnjoinedProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "absent-source", "joined-project")
	spoke := newMoveDeliverySpoke(t, hub, projects[1:])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "inbound", Author: "worker"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	later, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "later", Author: "worker"})
	require.NoError(t, err)

	require.NoError(t, spoke.sync(t, 0))

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
	got, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[0].ID, got.ProjectID)
	require.Equal(t, "inbound", got.Title)
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

// A spoke joined to one project keeps pulling when an issue moves out to a
// project the spoke never joined (#1046).
func TestSyncFederationPullsMoveToUnjoinedProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "joined-project", "absent-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "outbound", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	later, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "later", Author: "worker"})
	require.NoError(t, err)

	require.NoError(t, spoke.sync(t, 0))

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	issues, err := spoke.env.DB.ListIssues(ctx, db.ListIssuesParams{ProjectID: spoke.projects[0].ID})
	require.NoError(t, err)
	for _, listed := range issues {
		require.NotEqual(t, issue.UID, listed.UID, "the moved issue leaves the joined project")
	}
}

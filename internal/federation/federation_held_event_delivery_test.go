package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationReturnedEditWhileAnotherHeld(t *testing.T) {
	t.Run("linked issues", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "reset-source", "reset-destination", "reset-absent")
		spoke := newMoveDeliverySpoke(t, hub, projects[:2])
		first, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "first", Author: "worker"})
		require.NoError(t, err)
		second, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "second", Author: "worker"})
		require.NoError(t, err)
		for i := range spoke.projects {
			require.NoError(t, spoke.sync(t, i))
		}
		localFirst, err := spoke.env.DB.IssueByUID(ctx, first.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localSecond, err := spoke.env.DB.IssueByUID(ctx, second.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, held, err := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: localFirst.ID, ToIssueID: localSecond.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: localFirst.ID, FromShortID: localFirst.ShortID, FromUID: localFirst.UID, ToShortID: localSecond.ShortID, ToUID: localSecond.UID, Actor: "worker"})
		require.NoError(t, err)
		firstAway, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: first.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: first.Revision, Actor: "worker"})
		require.NoError(t, err)
		secondAway, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: second.ID, FromProjectID: projects[0].ID, ToProjectID: projects[2].ID, IfMatchRev: second.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.ErrorContains(t, spoke.sync(t, 0), "federation_project_not_enrolled")
		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: first.ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: firstAway.Issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 1))
		require.ErrorContains(t, spoke.sync(t, 0), "federation_project_not_enrolled")
		_, err = hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{held.UID})
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: second.ID, FromProjectID: projects[2].ID, ToProjectID: projects[1].ID, IfMatchRev: secondAway.Issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 1))
		require.NoError(t, spoke.sync(t, 0))
		events, err := hub.DB.EventsByUIDs(ctx, projects[0].ID, []string{held.UID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, held.ContentHash, events[0].ContentHash)
	})
}

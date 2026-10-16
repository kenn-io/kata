package federation

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
)

func TestSyncFederationPurgeMoveWhileSourceOffline(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "purge-source", "purge-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "purged while offline", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, spoke.sync(t, i))
	}
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, _, _, err = hub.DB.SoftDeleteIssue(t.Context(), issue.ID, "worker")
	require.NoError(t, err)
	_, err = hub.DB.PurgeIssue(t.Context(), issue.ID, "worker", nil)
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound, "source-only reconnect must remove the purged issue even though the move history is gone")
}

package federation

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationInterruptedLeaveDoesNotBlockUnrelatedPush(t *testing.T) {
	for _, pending := range []string{"leave", "enrollment"} {
		t.Run(pending, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "active-source", "pending-source")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			credential := spoke.credentials[projects[1].UID]
			credential.LeavePending = pending == "leave"
			credential.RelayEnrollmentPending = pending == "enrollment"
			spoke.credentials[projects[1].UID] = credential
			issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Unrelated ordinary push", Author: "worker"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0), "an unfinished operation on another project must not stall ordinary work")
			retained, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[0].ID, retained.ProjectID)
			// The same pending credential is required for an explicit source move.
			local, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "Required source proof", Author: "worker"})
			require.NoError(t, err)
			spoke.move(t, local, 1, 0)
			err = spoke.sync(t, 0)
			require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized, "required move-source credentials remain strict")
		})
	}
}

func TestSyncFederationPendingProofCannotAuthorizeCurrentOwner(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "stale-source", "pending-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved before delayed work", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, spoke.sync(t, i))
	}
	local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Delayed work needing target authority"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	credential := spoke.credentials[projects[1].UID]
	credential.LeavePending = true
	spoke.credentials[projects[1].UID] = credential
	err = spoke.sync(t, 0)
	var status *HubStatusError
	require.ErrorAs(t, err, &status, "the hub, rather than an unrelated proof builder, rejects missing current-owner authority")
	if status == nil {
		t.Fatal("missing hub denial")
	}
	require.Equal(t, http.StatusForbidden, status.StatusCode)
	comments, err := hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationMoveAckRetryAfterSourceTeardown(t *testing.T) {
	for _, kind := range []string{"revoked", "archived"} {
		t.Run(kind, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "ack-source", "ack-target")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Committed move with lost acknowledgement", Author: "worker"})
			require.NoError(t, err)
			for i := range projects {
				require.NoError(t, spoke.sync(t, i))
			}
			local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			spoke.move(t, local, 0, 1)
			before, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_spoke_push_cursor_advance=error")
			require.Error(t, spoke.sync(t, 1), "simulate losing a committed move acknowledgement before storing its cursor")
			t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
			committed, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, committed.ProjectID)
			unacknowledged, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			require.Equal(t, before.PushCursorEventID, unacknowledged.PushCursorEventID)
			if kind == "revoked" {
				grant, err := hub.DB.AuthorizeFederationToken(t.Context(), spoke.credentials[projects[0].UID].Token, projects[0].ID, "push")
				require.NoError(t, err)
				require.NoError(t, hub.DB.RevokeFederationEnrollment(t.Context(), grant.ID))
			} else {
				_, _, err = hub.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: projects[0].ID, Actor: "worker", Force: true})
				require.NoError(t, err)
			}
			_, _, err = hub.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Destination work after commit"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 1), "valid destination authority must acknowledge the duplicate and resume pulling")
			after, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			require.Greater(t, after.PushCursorEventID, before.PushCursorEventID)
			comments, err := spoke.env.DB.CommentsByIssue(t.Context(), local.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "Destination work after commit", comments[0].Body)
		})
	}
}

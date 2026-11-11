package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
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
		})
	}
}

func TestFederationMoveSourceCredentialValidation(t *testing.T) {
	for _, kind := range []string{"actor", "project"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "active-source", "other-source")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			credential := spoke.credentials[projects[1].UID]
			switch kind {
			case "actor":
				credential.Actor = "other-worker"
			case "project":
				credential.HubProjectID = projects[1].ID + 100
			}
			spoke.credentials[projects[1].UID] = credential
			binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			local, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "Required source", Author: "worker"})
			require.NoError(t, err)
			move, err := spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: local.ID, FromProjectID: spoke.projects[1].ID, ToProjectID: spoke.projects[0].ID, IfMatchRev: local.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, err = federationMoveSourceTokens(ctx, spoke.env.DB, spoke.credentials, binding, hub.URL, move.Events, clientpkg.Opts{})
			switch kind {
			case "actor":
				require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
			case "project":
				require.ErrorContains(t, err, "move source credential project differs from binding")
			}
		})
	}
}

package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
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

func TestFederationMoveSourceCredentialValidation(t *testing.T) {
	for _, kind := range []string{"origin", "actor", "project", "binding-project", "signing"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "active-source", "other-source")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			credential := spoke.credentials[projects[1].UID]
			switch kind {
			case "origin":
				credential.HubURL = "https://other.example"
			case "actor":
				credential.Actor = "other-worker"
			case "project":
				credential.HubProjectID = projects[1].ID + 100
			case "binding-project":
				binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[1].ID)
				require.NoError(t, err)
				binding.HubProjectUID = "01HZNQ7VFPK1XGD8R5MABCD4EX"
				_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
				require.NoError(t, err)
			case "signing":
				credential.Signing = &signingpolicy.Source{HubURL: "https://other.example"}
			}
			spoke.credentials[projects[1].UID] = credential
			_, event, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Independent work", Author: "worker"})
			require.NoError(t, err)
			binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			tokens, err := federationMoveSourceTokens(ctx, spoke.env.DB, spoke.credentials, binding, hub.URL, []db.Event{event}, clientpkg.Opts{})
			if kind == "signing" {
				require.ErrorIs(t, err, signingpolicy.ErrInvalid)
				return
			}
			require.NoError(t, err)
			require.NotContains(t, tokens, projects[1].UID)
			local, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "Required source", Author: "worker"})
			require.NoError(t, err)
			move, err := spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: local.ID, FromProjectID: spoke.projects[1].ID, ToProjectID: spoke.projects[0].ID, IfMatchRev: local.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, err = federationMoveSourceTokens(ctx, spoke.env.DB, spoke.credentials, binding, hub.URL, move.Events, clientpkg.Opts{})
			switch kind {
			case "origin":
				require.ErrorContains(t, err, "move source credential origin differs from binding")
			case "actor", "binding-project":
				require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
			case "project":
				require.ErrorContains(t, err, "move source credential project differs from binding")
			}
		})
	}
}

func TestSyncFederationUnusableOptionalProofDoesNotBlockPush(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "active-source", "other-source")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	credential := spoke.credentials[projects[1].UID]
	credential.HubURL = "https://other.example"
	spoke.credentials[projects[1].UID] = credential
	issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Independent work", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
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
	require.ErrorContains(t, err, "pending federation events remain")
	require.ErrorContains(t, err, "matching push access", "a suppressed pending proof reports the required access")
	comments, err := hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

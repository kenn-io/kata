package federation

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The sender can still see the old owner when another joined spoke has already
// moved the issue on the hub. Supplemental proofs cannot rely on local ownership.
func TestFederationOrdinarySourceProofIncludesJoinedCurrentProject(t *testing.T) {
	for _, kind := range []string{"same-origin", "other-binding-origin", "other-credential-origin", "other-actor", "pull-only"} {
		t.Run(kind, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "proof-source", "proof-target")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Stale local owner", Author: "worker"})
			require.NoError(t, err)
			_, event, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Delayed source edit"})
			require.NoError(t, err)
			other := testenv.New(t)
			target, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			switch kind {
			case "other-binding-origin":
				target.HubURL = other.URL
			case "other-credential-origin":
				credential := spoke.credentials[projects[1].UID]
				credential.HubURL = other.URL
				spoke.credentials[projects[1].UID] = credential
			case "other-actor":
				target.Actor = "other-worker"
			case "pull-only":
				target.PushEnabled = false
			}
			_, err = spoke.env.DB.UpsertFederationBinding(t.Context(), target)
			require.NoError(t, err)
			binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
			require.NoError(t, err)
			tokens, err := federationMoveSourceTokens(t.Context(), spoke.env.DB, spoke.credentials, binding, hub.URL, []db.Event{event}, clientpkg.Opts{})
			if kind == "other-credential-origin" {
				require.Error(t, err, "credentials are never sent to a different configured origin")
				require.Empty(t, tokens)
				return
			}
			require.NoError(t, err)
			if kind == "same-origin" {
				require.Equal(t, spoke.credentials[projects[1].UID].Token, tokens[projects[1].UID])
			} else {
				require.NotContains(t, tokens, projects[1].UID)
			}
		})
	}
}

func TestSyncFederationDelayedSourceEditCurrentProjectAuthority(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "delayed-edit-source", "delayed-edit-target")
	sender := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Delayed source work", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, sender.sync(t, i))
	}
	local, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, err = sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Authorized delayed edit"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	credential := sender.credentials[projects[1].UID]
	delete(sender.credentials, projects[1].UID)
	err = sender.sync(t, 0)
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	if status == nil {
		t.Fatal("expected missing current-project authority to be rejected")
	}
	require.Equal(t, http.StatusForbidden, status.StatusCode)
	comments, err := hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
	sender.credentials[projects[1].UID] = credential
	require.NoError(t, sender.sync(t, 0), "joined current-project proof allows retry without rewriting the source envelope")
	comments, err = hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "Authorized delayed edit", comments[0].Body)
}

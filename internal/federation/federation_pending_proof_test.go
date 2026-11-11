package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

func TestFederationMoveSourceCredentialValidation(t *testing.T) {
	for _, kind := range []string{"origin", "actor", "project", "signing"} {
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
			case "signing":
				credential.Signing = &signingpolicy.Source{HubURL: "https://other.example"}
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
			case "origin":
				require.ErrorContains(t, err, "move source credential origin differs from binding")
			case "actor":
				require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
			case "project":
				require.ErrorContains(t, err, "move source credential project differs from binding")
			case "signing":
				require.ErrorIs(t, err, signingpolicy.ErrInvalid)
			}
		})
	}
}

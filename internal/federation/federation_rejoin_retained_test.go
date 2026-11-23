package federation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestLegacyBindingKeepsNewWrongActorQuarantined(t *testing.T) {
	for _, timing := range []string{"before rejoin", "at rejoin", "after rejoin"} {
		t.Run(timing, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "rejoin-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			_, bad, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "wrong actor", Author: "old-actor"})
			require.NoError(t, err)
			createdAt := bad.CreatedAt
			switch timing {
			case "before rejoin":
				createdAt = createdAt.Add(time.Millisecond)
			case "after rejoin":
				createdAt = createdAt.Add(-time.Millisecond)
			}
			_, err = spoke.env.DB.ExecContext(ctx, `UPDATE federation_bindings SET created_at = ? WHERE project_id = ?`, createdAt.UTC().Format(time.RFC3339Nano), binding.ProjectID)
			require.NoError(t, err)
			binding.Actor = "new-actor"
			_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull", Actor: "new-actor"})
			require.NoError(t, err)
			spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor"}
			require.ErrorContains(t, spoke.sync(t, 0), "does not match bound actor")
			binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			require.Zero(t, binding.RetainedThroughEventID)
			if timing == "before rejoin" {
				later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: "new-actor"})
				require.NoError(t, err)
				require.NoError(t, spoke.sync(t, 0))
				_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, spoke.sync(t, 0), ErrFederationPushQuarantined)
			}
		})
	}
}

func rejoinWithActor(t *testing.T, spoke moveDeliverySpoke, hubURL string, hubProject db.Project, hubStore db.Storage, capabilities string) {
	t.Helper()
	ctx := t.Context()
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hubStore.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &hubProject.ID, Capabilities: capabilities, Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hubURL, HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[hubProject.UID] = config.FederationCredential{HubURL: hubURL, HubProjectID: hubProject.ID, Token: grant.Token, Actor: "new-actor"}
}

func TestRejoinRejectsWrongActor(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "wrong-actor-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	binding.Actor = "old-actor"
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, wrong, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "wrong actor task", Author: "old-actor"})
	require.NoError(t, err)
	require.Equal(t, "old-actor", wrong.Actor)
	require.Greater(t, wrong.ID, binding.RetainedThroughEventID)
	binding.Actor = "new-actor"
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	err = spoke.sync(t, 0)
	require.ErrorContains(t, err, "does not match bound actor")
	_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
}

func pushRejoin(t *testing.T, spoke moveDeliverySpoke) error {
	t.Helper()
	ctx := t.Context()
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	if err != nil {
		return err
	}
	creds := spoke.credentials[spoke.projects[0].UID]
	client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
	if err != nil {
		return err
	}
	return pushDirectSpoke(ctx, spoke.env.DB, &binding, creds.HubProjectID, client, nil, nil, spoke.credentials, clientpkg.Opts{})
}

func TestRejoinUnknownIssueStaysLocalAndLaterTaskPublishes(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	old, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local edit"})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	_, err = hub.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

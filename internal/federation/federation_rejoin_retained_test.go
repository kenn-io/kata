package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestRejoinKeepsOldActorEventsLocal(t *testing.T) {
	for _, pushEnabled := range []bool{true, false} {
		name := "push-enabled rejoin"
		if !pushEnabled {
			name = "enable push after rejoin"
		}
		t.Run(name, func(t *testing.T) { testRejoinKeepsOldActorEventsLocal(t, pushEnabled) })
	}
}

func testRejoinKeepsOldActorEventsLocal(t *testing.T, pushEnabled bool) {
	t.Helper()
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	eligible, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "eligible task", Author: "new-actor"})
	require.NoError(t, err)
	old, event, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "pull,push", Actor: "new-actor"})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, projects[0].ID)
	require.NoError(t, err)
	var replica api.CreateFederationReplicaResponseBody
	postJSON(t, spoke.env.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url": hub.URL, "hub_project_id": projects[0].ID, "hub_project_uid": projects[0].UID,
		"project_name": project.Name, "replay_horizon_event_id": hubBinding.ReplayHorizonEventID, "token": grant.Token, "capabilities": "pull,push", "actor": "new-actor", "push_enabled": pushEnabled,
	}, &replica)
	if !pushEnabled {
		cursor, err := spoke.env.DB.MaxEventID(ctx)
		require.NoError(t, err)
		_, err = spoke.env.DB.EnableFederationPush(ctx, project.ID, cursor)
		require.NoError(t, err)
	}
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local comment"})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "pull,push"}
	require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
	_, err = hub.DB.IssueByUID(ctx, eligible.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	events, err := spoke.env.DB.EventsByUIDs(ctx, project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{event}, events)
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	err = spoke.env.DB.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, spoke.env.DB.InstanceUID(), binding.PushCursorEventID)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
}

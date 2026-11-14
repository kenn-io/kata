package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
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

func TestSyncFederationMoveBackfillsLocalCreationAfterSourceLeave(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "local-history-source", "local-history-target")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "locally created history", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, _, err = hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[1].ID, Title: "destination cursor advances", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 1))
	current, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: current.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.env.DB.LeaveFederationReplica(t.Context(), spoke.projects[0].ID)
	require.NoError(t, err)
	delete(spoke.credentials, projects[0].UID)
	require.NoError(t, spoke.sync(t, 1), "recovered local history must not require its retired source binding")
	moved, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[1].ID, moved.ProjectID)
	require.Equal(t, issue.Title, moved.Title)
}

func TestSyncFederationMoveBackfillsLocalCreationAfterSourcePurge(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "local-history-source", "local-history-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "locally created history", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	current, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: current.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	delete(spoke.credentials, projects[0].UID)
	_, _, err = spoke.env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: spoke.projects[0].ID, Actor: "worker", Force: true})
	require.NoError(t, err)
	_, err = spoke.env.DB.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: spoke.projects[0].ID, Actor: "worker"})
	require.NoError(t, err)
	target, err := spoke.env.DB.CreateProjectWithUID(ctx, projects[1].Name, projects[1].UID)
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: target.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	spoke.projects = append(spoke.projects, target)
	spoke.credentials[target.UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "worker"}
	require.NoError(t, spoke.sync(t, 1), "recovered local history must not require its purged source")
	got, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	require.Equal(t, issue.Title, got.Title)
}

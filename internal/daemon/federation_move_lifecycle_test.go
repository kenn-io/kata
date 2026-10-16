package daemon_test

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestPrepareFederationLeaveRetainsPendingMoveSourceCredential(t *testing.T) {
	ctx := t.Context()
	store := openReplicaServiceStore(t)
	source, err := store.CreateProject(ctx, "leave-move-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "leave-move-target")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
	}
	credentials := newReplicaCredentialStore()
	credential := config.FederationCredential{HubURL: "https://hub.example", HubProjectID: source.ID, Actor: "worker", Token: "pending-move-source", ManagedByConfig: true, SpokeProjectName: source.Name}
	require.NoError(t, credentials.ReserveManagedFederationCredential(ctx, config.FederationManagedCredentialReservation{ProjectUID: source.UID, Credential: credential}))
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "pending move", Author: "worker"})
	require.NoError(t, err)
	move, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = daemon.PrepareFederationReplicaLeave(ctx, store, credentials, source.ID)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	retained, found, err := credentials.FederationCredential(ctx, source.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, retained.LeavePending, "refusal must precede the durable leave marker and hub revocation")
	require.Equal(t, credential.Token, retained.Token)
	require.False(t, daemon.FederationReplicaMappingSuppressed(store, source.Name))
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, move.EventID))
	_, err = daemon.PrepareFederationReplicaLeave(ctx, store, credentials, source.ID)
	require.NoError(t, err)
	retained, found, err = credentials.FederationCredential(ctx, source.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, retained.LeavePending)
}

func TestArchiveFederationMoveSourceReturnsSyncGuidance(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	source, err := env.DB.CreateProject(ctx, "archive-move-source")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(ctx, "archive-move-target")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err := env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
	}
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "pending archive move", Author: "worker"})
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/api/v1/projects/%d?actor=worker", env.URL, source.ID), nil)
	require.NoError(t, err)
	response, err := env.HTTP.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode, string(body))
	require.Contains(t, string(body), "federation_lifecycle_blocked")
	require.Contains(t, string(body), "sync")
}

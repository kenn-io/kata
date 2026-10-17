package daemon_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
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

// The credential write is exactly the former gap between native lifecycle
// validation and irrevocable hub teardown. A second DB handle also proves the
// move fence survives without the daemon's in-memory transition registry.
func TestPrepareFederationLeaveFencesConcurrentNativeMove(t *testing.T) {
	for _, side := range []string{"source", "destination", "failed-credential-mark"} {
		t.Run(side, func(t *testing.T) {
			ctx := t.Context()
			store := openReplicaServiceStore(t)
			source, err := store.CreateProject(ctx, "prepare-race-source")
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "prepare-race-target")
			require.NoError(t, err)
			for _, project := range []db.Project{source, target} {
				_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "leave preparation race", Author: "worker"})
			require.NoError(t, err)
			leaving := source
			if side == "destination" {
				leaving = target
			}
			credentials := &prepareLeaveFailureCredentialStore{replicaCredentialStore: newReplicaCredentialStore()}
			credential := config.FederationCredential{HubURL: "https://hub.example", HubProjectID: leaving.ID, Actor: "worker", Token: "prepare-race-token", ManagedByConfig: true, SpokeProjectName: leaving.Name}
			require.NoError(t, credentials.ReserveManagedFederationCredential(ctx, config.FederationManagedCredentialReservation{ProjectUID: leaving.UID, Credential: credential}))
			other, err := sqlitestore.Open(ctx, store.Path())
			require.NoError(t, err)
			t.Cleanup(func() { _ = other.Close() })
			move := db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"}
			var moveErr error
			credentials.onReplace = func() {
				_, moveErr = other.MoveIssueProject(ctx, move)
			}
			if side == "failed-credential-mark" {
				credentials.managedErr = errors.New("injected credential mark failure")
			}
			_, err = daemon.PrepareFederationReplicaLeave(ctx, store, credentials, leaving.ID)
			if side == "failed-credential-mark" {
				require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialIO)
				require.Contains(t, err.Error(), "retry", "a retained safe write fence needs actionable recovery guidance")
				retained, found, readErr := credentials.FederationCredential(ctx, leaving.UID)
				require.NoError(t, readErr)
				require.True(t, found)
				require.False(t, retained.LeavePending, "failed preparation must not authorize hub revocation")
				require.False(t, daemon.FederationReplicaMappingSuppressed(store, leaving.Name))
				_, readErr = other.MoveIssueProject(ctx, move)
				require.ErrorIs(t, readErr, db.ErrFederatedReadOnly, "the safe fence persists until preparation is retried")
				credentials.managedErr = nil
				credentials.onReplace = nil
				_, err = daemon.PrepareFederationReplicaLeave(ctx, store, credentials, leaving.ID)
			}
			require.NoError(t, err)
			require.ErrorIs(t, moveErr, db.ErrFederatedReadOnly, "a move must be fenced before credential preparation can authorize revocation")
			_, err = other.MoveIssueProject(ctx, move)
			require.ErrorIs(t, err, db.ErrFederatedReadOnly, "the native DB fence survives a second handle/process")
			current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, source.ID, current.ProjectID)
			_, err = daemon.LeaveFederationReplica(ctx, store, credentials, nil, leaving.ID)
			require.NoError(t, err, "a prepared leave cannot strand a new pending move")
		})
	}
}

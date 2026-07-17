package kata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestServiceRunWakesFederationOnCommittedEvent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	hubService, err := New(ctx, Config{
		DSN:  filepath.Join(t.TempDir(), "hub.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, hubService.Close()) })
	hubServer := httptest.NewServer(hubService.Handler())
	t.Cleanup(hubServer.Close)

	hubProject, err := hubService.store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	hubBinding, err := hubService.store.EnableProjectFederation(ctx, hubProject.ID, "example-user")
	require.NoError(t, err)

	spokeService, err := New(ctx, Config{
		DSN:  filepath.Join(t.TempDir(), "spoke.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spokeService.Close()) })
	spokeProject, err := spokeService.store.CreateProjectWithUID(ctx, "spoke-project", hubProject.UID)
	require.NoError(t, err)
	enrollment, err := hubService.store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: spokeService.store.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
		Actor:            "example-user",
	})
	require.NoError(t, err)

	offlineHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(offlineHub.Close)
	_, err = spokeService.store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               offlineHub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: hubBinding.ReplayHorizonEventID,
		PushEnabled:          true,
		Actor:                "example-user",
		AllowInsecure:        true,
		Enabled:              true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL: offlineHub.URL, HubProjectID: hubProject.ID,
		Token: enrollment.Token, AllowInsecure: true,
	}))

	runCtx, cancelRun := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- spokeService.Run(runCtx) }()
	t.Cleanup(func() {
		cancelRun()
		require.NoError(t, <-runDone)
	})
	require.Eventually(t, func() bool {
		status, statusErr := spokeService.store.FederationSyncStatusByProject(ctx, spokeProject.ID)
		return statusErr == nil && status.LastError != nil
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL: hubServer.URL, HubProjectID: hubProject.ID,
		Token: enrollment.Token, AllowInsecure: true,
	}))
	issue, event, err := spokeService.store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: spokeProject.ID, Title: "wake federation", Author: "example-user",
	})
	require.NoError(t, err)
	spokeService.broadcaster.Broadcast(daemon.StreamMsg{
		Kind: "event", Event: &event, ProjectID: spokeProject.ID,
	})

	require.Eventually(t, func() bool {
		_, issueErr := hubService.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		return issueErr == nil
	}, 2*time.Second, 10*time.Millisecond, "federation push should be event-driven, not wait for the 30-second poll")
}

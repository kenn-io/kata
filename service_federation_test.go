package kata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
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

	credentials := newServiceFederationCredentialStore()
	spokeService, err := New(ctx, Config{
		DSN:                   filepath.Join(t.TempDir(), "spoke.db"),
		Auth:                  AuthConfig{TrustCallerAuthentication: true},
		FederationCredentials: credentials,
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
	require.NoError(t, credentials.StoreFederationCredential(ctx, spokeProject.UID, FederationCredential{
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

	require.NoError(t, credentials.StoreFederationCredential(ctx, spokeProject.UID, FederationCredential{
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

func TestServicesIsolateFederationCredentialsForSharedProjectUID(t *testing.T) {
	ctx := context.Background()
	projectUID := "01HZNQ7VFPK1XGD8R5MABCD4EX"
	capabilities := []string{"pull", "pull,push"}
	services := make([]*Service, len(capabilities))
	projectIDs := make([]int64, len(capabilities))
	for i := range capabilities {
		service, err := New(ctx, Config{
			DSN:  filepath.Join(t.TempDir(), "service.db"),
			Auth: AuthConfig{TrustCallerAuthentication: true},
		})
		require.NoError(t, err)
		services[i] = service
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		requestBody, err := json.Marshal(map[string]any{
			"hub_url":                 "https://hub.example",
			"hub_project_id":          42,
			"hub_project_uid":         projectUID,
			"project_name":            "shared-project",
			"replay_horizon_event_id": 1,
			"actor":                   "example-user",
			"token":                   "token-" + capabilities[i],
			"capabilities":            capabilities[i],
		})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/federation/replicas", bytes.NewReader(requestBody))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code)
		var created api.CreateFederationReplicaBody
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
		projectIDs[i] = created.Project.ID
	}

	for i := range services {
		statuses := serviceFederationStatuses(t, services[i])
		require.Len(t, statuses, 1)
		assert.Equal(t, capabilities[i], statuses[0].Capabilities)
	}

	leaveBody, err := json.Marshal(map[string]any{"actor": "example-user"})
	require.NoError(t, err)
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/federation/replicas/"+fmt.Sprint(projectIDs[0])+"/actions/leave",
		bytes.NewReader(leaveBody),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	services[0].Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Empty(t, serviceFederationStatuses(t, services[0]))
	secondStatuses := serviceFederationStatuses(t, services[1])
	require.Len(t, secondStatuses, 1)
	assert.Equal(t, capabilities[1], secondStatuses[0].Capabilities)
}

func serviceFederationStatuses(t *testing.T, service *Service) []api.FederationProjectStatus {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/federation/status", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var body api.FederationStatusBody
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	return body.Statuses
}

type serviceFederationCredentialStore struct {
	mu      sync.Mutex
	entries map[string]FederationCredential
}

func newServiceFederationCredentialStore() *serviceFederationCredentialStore {
	return &serviceFederationCredentialStore{entries: make(map[string]FederationCredential)}
}

func (s *serviceFederationCredentialStore) FederationCredential(
	_ context.Context, projectUID string,
) (FederationCredential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, ok := s.entries[projectUID]
	return credential, ok, nil
}

func (s *serviceFederationCredentialStore) StoreFederationCredential(
	_ context.Context, projectUID string, credential FederationCredential,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[projectUID] = credential
	return nil
}

func (s *serviceFederationCredentialStore) DeleteFederationCredential(
	_ context.Context, projectUID string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, projectUID)
	return nil
}

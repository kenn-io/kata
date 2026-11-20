package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Both federation status routes show a spoke binding the runner is holding
// back, and why, and leave other bindings without a retry block.
func TestFederationStatusShowsSyncRetry(t *testing.T) {
	ctx := t.Context()
	t.Setenv("KATA_HOME", t.TempDir())
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	newSpoke := func(name string, hubProjectID int64) db.Project {
		project, err := store.CreateProject(ctx, name)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
			HubProjectID: hubProjectID, HubProjectUID: project.UID, ReplayHorizonEventID: 1, Enabled: true,
		})
		require.NoError(t, err)
		return project
	}
	refused := newSpoke("refused-project", 42)
	healthy := newSpoke("healthy-project", 43)
	next := time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)
	retry := api.FederationSyncRetry{Reason: "refused", HTTPStatus: http.StatusForbidden, Failures: 5, NextAttemptAt: next}

	server := daemon.NewServer(daemon.ServerConfig{
		DB:   store,
		Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true},
		FederationSyncRetry: func(projectID int64) (api.FederationSyncRetry, bool) {
			return retry, projectID == refused.ID
		},
	})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	read := func(path string) map[int64]api.FederationProjectStatus {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer bootstrap-test-token")
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var body api.FederationStatusBody
		require.NoError(t, json.UnmarshalRead(response.Body, &body))
		statuses := map[int64]api.FederationProjectStatus{}
		for _, status := range body.Statuses {
			statuses[status.ProjectID] = status
		}
		return statuses
	}

	all := read("/api/v1/federation/status")
	require.Equal(t, &retry, all[refused.ID].SyncRetry)
	require.Nil(t, all[healthy.ID].SyncRetry)
	one := read(fmt.Sprintf("/api/v1/projects/%d/federation/status?omit_errors=true", refused.ID))
	require.Equal(t, &retry, one[refused.ID].SyncRetry, "the schedule is not error text")
}

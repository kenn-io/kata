package daemon_test

import (
	"encoding/json/v2"
	"errors"
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

// A compact project status, which the browser reads, says a sync error exists
// without its text; the full status keeps the detail for the CLI.
func TestFederationCompactStatusWithholdsSyncErrorText(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, store.RecordFederationSyncError(ctx, project.ID, errors.New("example sync failure"), time.Now().UTC()))

	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	read := func(query string) api.FederationProjectStatus {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/status%s", httpServer.URL, project.ID, query), nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer bootstrap-test-token")
		response, err := httpServer.Client().Do(request)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		require.Equal(t, http.StatusOK, response.StatusCode)
		var body api.FederationStatusBody
		require.NoError(t, json.UnmarshalRead(response.Body, &body))
		require.Len(t, body.Statuses, 1)
		return body.Statuses[0]
	}
	require.Equal(t, "example sync failure", *read("").LastError)
	compact := read("?compact=true")
	require.NotNil(t, compact.LastError)
	require.NotEmpty(t, *compact.LastError)
	require.NotContains(t, *compact.LastError, "example sync failure")
}

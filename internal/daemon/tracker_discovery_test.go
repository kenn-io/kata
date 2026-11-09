package daemon

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestTrackerDiscoveryUsesQualifiedGrantAndOmitsLocalSelectors(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	principal := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectStaticOwner, Subject: store.InstanceUID()}
	connection := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first", Provider: "notion", Label: "Example account", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_PRIVATE_SELECTOR"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: principal, Actions: []tracker.Action{tracker.ActionDiscover, tracker.ActionBind}, Sources: []string{"notion:source"}}}}
	runtime, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{connection}, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: principal, Actor: "member"}})
	require.NoError(t, err)
	server := NewServer(ServerConfig{DB: store, TrackerRuntime: runtime, Auth: config.AuthConfig{Token: "fixture-transport"}})
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1/api/v1/projects/%d/tracker-connections", project.ID), nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer fixture-transport")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), connection.UID)
	require.Contains(t, response.Body.String(), "Example account")
	for _, private := range []string{"EXAMPLE_PRIVATE_SELECTOR", "token_env", "request_digest", "grant"} {
		require.NotContains(t, response.Body.String(), private)
	}
	// A retired/suspended snapshot cannot keep advertising cached accounts.
	runtime.Suspend()
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
}

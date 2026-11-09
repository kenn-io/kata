package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

// MCP declares its client class on every tracker request. Scope and the actual
// REST credential remain intact; the actor option is not an authority header.
func TestTrackerMCPUsesAccountTransportAndScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/7/tracker-connections", r.URL.Path)
		require.Equal(t, "mcp", r.Header.Get("X-Kata-Tracker-Client"))
		require.Equal(t, "Bearer fixture-account", r.Header.Get("Authorization"))
		require.Equal(t, `["id:7"]`, r.Header.Get("X-Kata-Project-Scope"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"root_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","connections":[]}`))
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client(), kataclient.WithRequestEditor(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer fixture-account")
		return nil
	}))
	require.NoError(t, err)
	client, err = client.WithProjectScope([]string{"id:7"})
	require.NoError(t, err)
	scope, err := NewBoundScope(ProjectIdentity{ID: 7, Name: "tracker-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "display-only"}}
	_, out, err := handlers.trackerConnections(t.Context(), nil, TrackerConnectionsInput{})
	require.NoError(t, err)
	require.Empty(t, out.Connections)
}

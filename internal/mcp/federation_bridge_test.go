package mcpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// R9: MCP selects one local bridge and lets the daemon resolve saved hub
// credentials. Caller-supplied actor/token fields never enter this contract.
func TestFederationBridgeToolsMCP(t *testing.T) {
	calls := 0
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/v1/federation/bridges":
			require.Equal(t, http.MethodPost, r.Method)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "local-member", body["actor"])
			require.Equal(t, "shared-replica", body["project_name"])
			require.Equal(t, "example-hub", body["hub_catalog"])
			require.Equal(t, "shared-project", body["hub_project"])
			require.Equal(t, true, body["preflight"])
			require.Equal(t, false, body["serve_downstream"])
			require.NotContains(t, body, "token")
			writeJSON(w, map[string]any{"project_name": "shared-replica", "hub_project_uid": "00000000000000000000000003", "hub_project_id": 7, "hub_instance_uid": "00000000000000000000000002", "hub_catalog": "example-hub", "hub_url": "https://hub.example", "local_account": "local-member", "upstream_account": "member", "direction": "bidirectional", "status": "ready"})
		case "/api/v1/federation/bridges/shared-replica":
			require.Equal(t, http.MethodGet, r.Method)
			writeJSON(w, map[string]any{"project_name": "shared-replica", "project_uid": "00000000000000000000000003", "hub_catalog": "example-hub", "hub_url": "https://hub.example", "local_account": "local-member", "upstream_account": "member", "direction": "bidirectional", "state": "enrollment_pending", "credential_status": "enrollment_pending"})
		case "/api/v1/federation/bridges/shared-replica/disconnect":
			require.Equal(t, http.MethodPost, r.Method)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, true, body["preflight"])
			writeJSON(w, map[string]any{"project_name": "shared-replica", "project_uid": "00000000000000000000000003", "status": "ready"})
		default:
			t.Fatalf("unexpected bridge request %s", r.URL.Path)
		}
	})
	session := connectRawTestServerWithOptions(t, Options{Client: client, Scope: NewAllScope(), Actor: "local-member", Version: "test"})
	callAdministrationTool(t, session, "kata.load_federation", map[string]any{})
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"kata.federation_bridge_connect", map[string]any{"project": "shared-replica", "hub_catalog": "example-hub", "hub_project": "shared-project", "preflight": true, "serve_downstream": false}, "bidirectional"},
		{"kata.federation_bridge_status", map[string]any{"project": "shared-replica"}, "enrollment_pending"},
		{"kata.federation_bridge_disconnect", map[string]any{"project": "shared-replica", "preflight": true}, "ready"},
	} {
		out := callAdministrationTool(t, session, tc.name, tc.args)
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		require.Contains(t, string(raw), tc.want)
		require.NotContains(t, string(raw), "token")
	}
	require.Equal(t, 3, calls)
}

func TestFederationBridgeToolsRejectBoundScopeMCP(t *testing.T) {
	client := reviewClient(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("scoped bridge tool must not request %s", r.URL.Path)
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 7, Name: "shared-replica"})
	require.NoError(t, err)
	session := connectRawTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "local-member", Version: "test"})
	callAdministrationTool(t, session, "kata.load_federation", map[string]any{})
	for _, name := range []string{"kata.federation_bridge_connect", "kata.federation_bridge_status", "kata.federation_bridge_disconnect"} {
		args := map[string]any{"project": "shared-replica"}
		if name == "kata.federation_bridge_connect" {
			args["hub_catalog"] = "example-hub"
			args["hub_project"] = "shared-project"
		}
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, err)
		require.True(t, result.IsError)
		require.Contains(t, string(mustJSON(t, result)), "daemon-wide")
	}
}

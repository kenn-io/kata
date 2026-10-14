package mcpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// MCP repins one selected root project through the daemon and passes the
// preflight and previous key path through unchanged.
func TestFederationRootKeyRepinMCP(t *testing.T) {
	calls := 0
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/federation/root-keys/hub-project/repin", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, true, body["preflight"])
		require.Equal(t, "/srv/kata/previous-root.key", body["previous_key_file"])
		require.Equal(t, true, body["strand_relays"])
		writeJSON(w, map[string]any{"project_name": "hub-project", "project_uid": "00000000000000000000000003", "status": "ready", "method": "signed_rotation"})
	})
	session := connectRawTestServerWithOptions(t, Options{Client: client, Scope: NewAllScope(), Actor: "local-member", Version: "test"})
	callAdministrationTool(t, session, "kata.load_federation", map[string]any{})
	out := callAdministrationTool(t, session, "kata.federation_root_key_repin", map[string]any{"project": "hub-project", "preflight": true, "previous_key_file": "/srv/kata/previous-root.key", "strand_relays": true})
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(raw), "signed_rotation")
	require.Equal(t, 1, calls)
}

func TestFederationRootKeyRepinRejectsBoundScopeMCP(t *testing.T) {
	client := reviewClient(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("scoped repin must not request %s", r.URL.Path)
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 7, Name: "hub-project"})
	require.NoError(t, err)
	session := connectRawTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "local-member", Version: "test"})
	callAdministrationTool(t, session, "kata.load_federation", map[string]any{})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.federation_root_key_repin", Arguments: map[string]any{"project": "hub-project"}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, string(mustJSON(t, result)), "daemon-wide")
}

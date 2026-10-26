package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPToolSendsHeader(t *testing.T) {
	var headers []string
	var bodies []map[string]any
	health := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			health++
			writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.18.0"})
			return
		}
		headers = append(headers, r.Header.Get("X-Kata-Teammate"))
		if r.Method == http.MethodPost {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			bodies = append(bodies, body)
		}
		_, _ = w.Write(daemonResponse(r))
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "coordinator", Version: "test", Teammate: "pi.example", TeammateInferred: true})
	for _, tc := range []struct {
		tool, want string
		override   any
	}{
		{"kata.comment", "pi.example", nil}, {"kata.comment", "reviewer-7", "reviewer-7"},
		{"kata.comment", "", ""}, {"kata.comment", "pi.example", nil},
		{"kata.create", "pi.example", nil}, {"kata.show", "pi.example", nil},
	} {
		args := map[string]any{"ref": "abc1", "body": "progress", "idempotency_key": "header-" + tc.want}
		if tc.tool == "kata.create" {
			delete(args, "ref")
			args["title"] = "New issue"
		}
		if tc.tool == "kata.show" {
			args = map[string]any{"ref": "abc1"}
		}
		if tc.override != nil {
			args["teammate"] = tc.override
		}
		headers = nil
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tc.tool, Arguments: args})
		require.NoError(t, err)
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		require.NotEmpty(t, headers)
		for _, got := range headers {
			require.Equal(t, tc.want, got)
		}
	}
	require.Equal(t, 1, health, "only the explicit comment override needs preflight")
	require.NotContains(t, bodies[0], "teammate")
	require.Equal(t, "reviewer-7", bodies[1]["teammate"])
	require.NotContains(t, bodies[2], "teammate")
	require.NotContains(t, bodies[3], "teammate")
	metadata, _ := bodies[4]["metadata"].(map[string]any)
	require.NotContains(t, metadata, "teammate")
}

package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPSearchStatusCompatibilityAndForwarding(t *testing.T) {
	for _, version := range []string{"", "invalid", "0.18.0", "0.19.0"} {
		t.Run(version, func(t *testing.T) {
			var searches atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					writeJSON(w, map[string]any{"ok": true, "api_schema_version": version})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/search") {
					searches.Add(1)
					assert.Equal(t, "closed", r.URL.Query().Get("status"))
					assert.Equal(t, "4", r.URL.Query().Get("limit"))
					assert.Equal(t, "bug", r.URL.Query().Get("label"))
				}
				_, _ = w.Write(daemonResponse(r))
			}))
			t.Cleanup(daemon.Close)
			client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
			require.NoError(t, err)
			session := connectTestServerWithClient(t, client)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "work", "status": "closed", "limit": 3, "labels": []string{"bug"}}})
			require.NoError(t, err)
			if version == "0.19.0" {
				require.False(t, result.IsError, "%s", mustJSON(t, result))
				assert.EqualValues(t, 1, searches.Load())
			} else {
				require.True(t, result.IsError)
				assert.Contains(t, string(mustJSON(t, result)), "requires daemon API 0.19.0 or newer")
				assert.Zero(t, searches.Load())
			}
		})
	}
}

func TestMCPSearchStatusRejectsInvalidExplicitValues(t *testing.T) {
	var searches atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/search") {
			searches.Add(1)
		}
		_, _ = w.Write(daemonResponse(r))
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, client)
	for _, status := range []string{"", " ", "OPEN", "all", "pending"} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "work", "status": status}})
		require.NoError(t, err)
		require.True(t, result.IsError, "status=%q", status)
	}
	assert.Zero(t, searches.Load())
}

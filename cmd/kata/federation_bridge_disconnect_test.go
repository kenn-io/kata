package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// R9: the CLI selects one bridge on the local daemon and never reads or sends
// its upstream credential; preview is an explicit request to that same route.
func TestFederationBridgeDisconnectUsesLocalProject(t *testing.T) {
	for _, preflight := range []bool{false, true} {
		name := "disconnect"
		if preflight {
			name = "preflight"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/api/v1/federation/bridges/shared-replica/disconnect", r.URL.Path)
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, preflight, body["preflight"])
				require.NotContains(t, body, "token")
				status := "disconnected"
				if preflight {
					status = "ready"
				}
				writeFederationRebindJSON(t, w, map[string]any{"project_name": "shared-replica", "project_uid": "00000000000000000000000003", "status": status})
			})
			args := []string{"--json", "--project", "shared-replica", "federation", "bridge", "disconnect"}
			if preflight {
				args = append(args, "--preflight")
			}
			out, err := runFederationRebindAgainstServer(t, server.URL, args...)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Contains(t, out, "shared-replica")
			require.NotContains(t, out, "spoke-daemon-secret")
		})
	}
}

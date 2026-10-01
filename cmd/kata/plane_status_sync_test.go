package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlaneStatusSyncRequestsAndCapability(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		mode   any
		config map[string]any
	}{
		{"two-way", []string{"--status-sync=two-way", "--closed-state", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "--open-state="}, "two-way", map[string]any{"closed_state_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "open_state_id": ""}},
		{"one-way", []string{"--status-sync=one-way"}, "one-way", map[string]any{}},
		{"closed override", []string{"--closed-state", cliPlaneState}, nil, map[string]any{"closed_state_id": cliPlaneState}},
		{"open override", []string{"--open-state", cliPlaneState}, nil, map[string]any{"open_state_id": cliPlaneState}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, tc.mode, body["status_sync"])
				if len(tc.config) == 0 {
					require.Empty(t, body["config"])
				} else {
					require.Equal(t, tc.config, body["config"])
				}
				require.NoError(t, json.NewEncoder(w).Encode(cliPlaneResponse()))
			})
			args := append([]string{"sync", "plane", "enable"}, tc.args...)
			runCLI(t, env, dir, args...)
			env, dir = statusSyncCLIHTTP(t, false, func(http.ResponseWriter, *http.Request) {
				t.Error("unsupported status controls reached mutation endpoint")
			})
			_, _, err := runCLIWithErr(t, env, dir, args...)
			require.ErrorContains(t, err, "upgrade")
		})
	}
}

func TestPlaneStatusSyncOmittedControlsDoNotRequireCapability(t *testing.T) {
	env, dir := statusSyncCLIHTTP(t, false, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.NotContains(t, body, "status_sync")
		require.Empty(t, body["config"])
		require.NoError(t, json.NewEncoder(w).Encode(cliPlaneResponse()))
	})
	out := runCLI(t, env, dir, "sync", "plane", "enable")
	require.Contains(t, out, "Status sync: one-way")
}

func TestPlaneStatusSyncInvalidFlags(t *testing.T) {
	for _, args := range [][]string{{"--status-sync="}, {"--status-sync=invalid"}, {"--closed-state=bad"}, {"--open-state=https://plane.example/state"}} {
		env, dir := notionCLIHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid options reached mutation endpoint") })
		_, _, err := runCLIWithErr(t, env, dir, append([]string{"sync", "plane", "enable"}, args...)...)
		_ = requireCLIError(t, err, ExitValidation)
	}
}

func TestPlaneStatusSyncOutput(t *testing.T) {
	for _, action := range []string{"status", "once"} {
		for _, mode := range []string{"human", "agent", "json"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
					response := cliPlaneResponse()
					response["status"].(map[string]any)["status_sync"] = "two-way"
					response["status"].(map[string]any)["pending_count"] = 3
					response["binding"].(map[string]any)["config"].(map[string]any)["closed_state_id"] = cliPlaneState
					response["status_updated"] = 2
					require.NoError(t, json.NewEncoder(w).Encode(response))
				})
				args := []string{"sync", "plane", action}
				if mode != "human" {
					args = append([]string{"--" + mode}, args...)
				}
				out := runCLI(t, env, dir, args...)
				if mode == "json" {
					var body map[string]any
					require.NoError(t, json.Unmarshal([]byte(out), &body))
					require.Equal(t, "two-way", body["status"].(map[string]any)["status_sync"])
					require.Equal(t, float64(3), body["status"].(map[string]any)["pending_count"])
				} else if mode == "agent" {
					require.Contains(t, out, "status_sync=two-way")
					require.Contains(t, out, "pending_count=3")
					require.Contains(t, out, "closed_state_id="+cliPlaneState)
				} else if action == "status" {
					require.Contains(t, out, "Status sync: two-way")
					require.Contains(t, out, "Pending status changes: 3")
					require.Contains(t, out, "Closed target: "+cliPlaneState)
				}
				if action == "once" && mode != "json" {
					require.Contains(t, out, "status_updated=2")
				}
			})
		}
	}
}

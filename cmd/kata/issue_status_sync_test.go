package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func statusSyncCLIHTTP(t *testing.T, supported bool, handler http.HandlerFunc) (*testenv.Env, string) {
	t.Helper()
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"issue_status_sync": supported}))
			return
		}
		if strings.Contains(r.URL.Path, "/issue-sync/") {
			handler(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return &testenv.Env{URL: server.URL}, dir
}

func TestIssueStatusSyncCLIRequestsAndCapability(t *testing.T) {
	for _, provider := range []string{"github", "notion"} {
		t.Run(provider, func(t *testing.T) {
			env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "two-way", body["status_sync"])
				require.NotContains(t, body["config"], "status_sync")
				if provider == "notion" {
					require.Equal(t, "Complete", body["config"].(map[string]any)["complete_group"])
					require.Equal(t, "", body["config"].(map[string]any)["closed_status"])
				}
				require.NoError(t, json.NewEncoder(w).Encode(cliNotionResponse()))
			})
			args := []string{"sync", provider, "enable", "--status-sync=two-way"}
			if provider == "github" {
				args = append(args, "--repo", "example-owner/example-repo")
			} else {
				args = append(args, "--complete-group", "Complete", "--closed-status=")
			}
			runCLI(t, env, dir, args...)
			env, dir = statusSyncCLIHTTP(t, false, func(http.ResponseWriter, *http.Request) { t.Error("unsupported mode request reached daemon") })
			_, _, err := runCLIWithErr(t, env, dir, args...)
			require.Error(t, err)
			require.Contains(t, err.Error(), "upgrade")
		})
	}
}

func TestNotionTwoWayRejectsExplicitDoneStatus(t *testing.T) {
	env, dir := statusSyncCLIHTTP(t, true, func(http.ResponseWriter, *http.Request) { t.Error("invalid options reached daemon") })
	_, _, err := runCLIWithErr(t, env, dir, "sync", "notion", "enable", "--status-sync=two-way", "--done-status", "Delivered")
	_ = requireCLIError(t, err, ExitValidation)
}

func TestIssueStatusSyncCLIStatusOutput(t *testing.T) {
	for _, provider := range []string{"github", "notion"} {
		for _, mode := range []string{"human", "agent"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, _ *http.Request) {
					response := cliNotionResponse()
					response["status"].(map[string]any)["status_sync"] = "two-way"
					response["status"].(map[string]any)["pending_count"] = 3
					require.NoError(t, json.NewEncoder(w).Encode(response))
				})
				args := []string{"sync", provider, "status"}
				if mode == "agent" {
					args = append([]string{"--agent"}, args...)
				}
				out := runCLI(t, env, dir, args...)
				if mode == "agent" {
					require.Contains(t, out, "status_sync=two-way")
					require.Contains(t, out, "pending_count=3")
				} else {
					require.Contains(t, out, "Status sync: two-way")
					require.Contains(t, out, "Pending status changes: 3")
				}
			})
		}
	}
}

func TestNotionStatusSyncGroupDefaultsOutput(t *testing.T) {
	env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, _ *http.Request) {
		response := cliNotionResponse()
		config := response["binding"].(map[string]any)["config"].(map[string]any)
		delete(config, "done_status_ids")
		config["complete_group_id"] = "complete-group"
		config["todo_group_id"] = "todo-group"
		config["open_status_id"] = "ready"
		require.NoError(t, json.NewEncoder(w).Encode(response))
	})
	out := runCLI(t, env, dir, "sync", "notion", "status")
	require.Contains(t, out, "Complete group: complete-group")
	require.Contains(t, out, "To-do group: todo-group")
	require.Contains(t, out, "Closed target: live group default")
	require.Contains(t, out, "Open target: ready")
}

func TestIssueStatusSyncCLIOmittedModePreservesRequest(t *testing.T) {
	for _, provider := range []string{"github", "notion"} {
		t.Run(provider, func(t *testing.T) {
			env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.NotContains(t, body, "status_sync")
				require.NoError(t, json.NewEncoder(w).Encode(cliNotionResponse()))
			})
			args := []string{"sync", provider, "enable"}
			if provider == "github" {
				args = append(args, "--repo", "example-owner/example-repo")
			}
			runCLI(t, env, dir, args...)
		})
	}
}

func TestIssueStatusSyncCLIInvalidMode(t *testing.T) {
	for _, provider := range []string{"github", "notion"} {
		for _, mode := range []string{"", "invalid"} {
			env, dir := statusSyncCLIHTTP(t, true, func(http.ResponseWriter, *http.Request) { t.Error("invalid mode reached daemon") })
			_, _, err := runCLIWithErr(t, env, dir, "sync", provider, "enable", "--status-sync="+mode)
			_ = requireCLIError(t, err, ExitValidation)
		}
	}
}

func TestIssueStatusSyncOnceOutputCountsInwardTransitions(t *testing.T) {
	for _, provider := range []string{"github", "notion"} {
		for _, mode := range []string{"human", "agent"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, _ *http.Request) {
					response := cliNotionResponse()
					response["status_updated"] = 1
					require.NoError(t, json.NewEncoder(w).Encode(response))
				})
				args := []string{"sync", provider, "once"}
				if mode == "agent" {
					args = append([]string{"--agent"}, args...)
				}
				out := runCLI(t, env, dir, args...)
				require.Contains(t, out, "status_updated=1")
			})
		}
	}
}

func TestGitHubStatusSyncEnableShowsMode(t *testing.T) {
	env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, _ *http.Request) {
		response := cliNotionResponse()
		response["status"].(map[string]any)["status_sync"] = "two-way"
		require.NoError(t, json.NewEncoder(w).Encode(response))
	})
	out := runCLI(t, env, dir, "sync", "github", "enable", "--repo", "example-owner/example-repo", "--status-sync=two-way")
	require.Contains(t, out, "Status sync: two-way")
}

func TestNotionStatusSyncDefaultRequiresCorrespondingGroup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		todo     bool
		wantOpen bool
	}{{"complete only", false, false}, {"both groups", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir := statusSyncCLIHTTP(t, true, func(w http.ResponseWriter, _ *http.Request) {
				response := cliNotionResponse()
				config := response["binding"].(map[string]any)["config"].(map[string]any)
				delete(config, "done_status_ids")
				config["complete_group_id"] = "complete-group"
				if tc.todo {
					config["todo_group_id"] = "todo-group"
				}
				require.NoError(t, json.NewEncoder(w).Encode(response))
			})
			out := runCLI(t, env, dir, "sync", "notion", "status")
			require.Contains(t, out, "Closed target: live group default")
			if tc.wantOpen {
				require.Contains(t, out, "Open target: live group default")
			} else {
				require.NotContains(t, out, "Open target:")
			}
		})
	}
}

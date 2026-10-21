package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/version"
)

func TestCLIActionHeadersAndTelemetryAdmission(t *testing.T) {
	for _, scenario := range []string{"release", "KATA_TELEMETRY_ENABLED", "dev"} {
		t.Run(scenario, func(t *testing.T) {
			saved := version.Version
			t.Cleanup(func() { version.Version = saved })
			switch scenario {
			case "KATA_TELEMETRY_ENABLED":
				t.Setenv(scenario, "0")
			case "dev":
				version.Version = "dev"
			}
			env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = &cliUseTelemetry{} })
			ref := createIssue(t, env, pid, "Close target")
			target, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
			var mu sync.Mutex
			var surfaces []string
			var telemetryPosts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/ui/telemetry" {
					mu.Lock()
					telemetryPosts++
					mu.Unlock()
				}
				if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/issues") || strings.HasSuffix(r.URL.Path, "/actions/close")) {
					mu.Lock()
					surfaces = append(surfaces, r.Header.Get("X-Kata-Surface"))
					mu.Unlock()
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			env.URL = server.URL
			stubIsTTY(t, true)
			t.Setenv(hooks.HookVersionEnv, "")
			t.Cleanup(func() { cliUseTarget.Store(nil) })
			settledCLIUse(t, func(t *testing.T) {
				_, err = runCmdOutput(t, env, "create", "Example task", "--workspace", dir)
				require.NoError(t, err)
				_, err = runCmdOutput(t, env, "close", ref, "--workspace", dir, "--done", "--message", "Completed the requested work and checked the result.", "--commit", "abc1234")
				require.NoError(t, err)
				_, err = runCmdOutput(t, env, "agent-contract-hook")
				require.NoError(t, err)
			})
			mu.Lock()
			defer mu.Unlock()
			if scenario == "release" {
				assert.Equal(t, []string{"cli", "cli"}, surfaces)
				require.Positive(t, telemetryPosts)
			} else {
				assert.Equal(t, []string{"", ""}, surfaces)
				require.Zero(t, telemetryPosts)
			}
		})
	}
}

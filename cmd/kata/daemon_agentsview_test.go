package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsview"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
)

func TestReloadAgentsViewKeepsChangedTargetAndRefreshesCredential(t *testing.T) {
	home := t.TempDir()
	headers := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"session_not_found","message":"not found"}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("EXAMPLE_USAGE_TOKEN", "first")
	initial := config.AgentsViewConfig{URL: upstream.URL, TokenEnv: "EXAMPLE_USAGE_TOKEN"}
	resolved, err := initial.Resolve(t.Context(), false)
	require.NoError(t, err)
	service, err := agentsview.NewService(resolved, false)
	require.NoError(t, err)
	// A changed target never resolves its newly selected secret and keeps the running client.
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[agentsview]\nurl = \"https://other.example\"\ntoken_file = \"/missing\"\n"), 0600))
	changed, err := reloadAgentsView(t.Context(), home, service, false)
	require.NoError(t, err)
	require.False(t, changed)
	require.True(t, service.Enabled())
	// The same target re-resolves its credential, which the next upstream request carries.
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[agentsview]\nurl = \""+upstream.URL+"\"\ntoken_env = \"EXAMPLE_USAGE_TOKEN\"\n"), 0600))
	t.Setenv("EXAMPLE_USAGE_TOKEN", "second")
	changed, err = reloadAgentsView(t.Context(), home, service, false)
	require.NoError(t, err)
	require.True(t, changed)
	sessions := []cron.Session{{Agent: "example", SessionID: "id"}}
	result, err := service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
	require.NoError(t, err)
	require.Equal(t, "not_found", result.Sessions[0].State)
	require.Equal(t, "Bearer second", <-headers)
	t.Run("invalid credential keeps client", func(t *testing.T) {
		assertAgentsViewInvalidCredentialReloadKeepsClient(t)
	})
}

func TestAgentsViewDaemonStartupAndReloadLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("runs an isolated daemon")
	}
	binary := kataTestBinary(t, "agentsview-contract")
	childEnv := daemonLifecycleEnvironment(t)
	for _, tc := range []struct{ name, initial, startup string }{
		{"off", "", "AgentsView usage: off"},
		{"on", "[agentsview]\nurl = 'https://agentsview.example'\n", "AgentsView usage: on (agentsview.example)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			workspace := t.TempDir()
			env := append(append([]string(nil), childEnv...), "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"), "KATA_WORKSPACE="+workspace)
			configPath := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(configPath, []byte(tc.initial), 0o600))
			logfile := filepath.Join(home, "fixture.log")
			log, err := os.Create(logfile)
			require.NoError(t, err)
			t.Cleanup(func() { _ = log.Close() })
			process := exec.CommandContext(context.WithoutCancel(t.Context()), binary, "daemon", "start", "--foreground", "--listen", "127.0.0.1:0") //nolint:gosec // isolated test-built daemon
			process.Env = env
			process.Stdout, process.Stderr = log, log
			require.NoError(t, process.Start())
			done := make(chan error, 1)
			go func() { done <- process.Wait() }()
			t.Cleanup(func() {
				_ = process.Process.Kill()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("daemon did not exit")
				}
			})
			readLog := func() string { data, _ := os.ReadFile(logfile); return string(data) }
			require.Eventually(t, func() bool {
				if !strings.Contains(readLog(), tc.startup) {
					return false
				}
				status := exec.CommandContext(t.Context(), binary, "daemon", "status") //nolint:gosec // isolated test-built CLI
				status.Env = env
				return status.Run() == nil
			}, 20*time.Second, 50*time.Millisecond) //nolint:kennlint // external daemon readiness and logs
			if tc.name == "off" {
				return
			}
			changed := "[agentsview]\nurl = 'https://other.example'\n"
			require.NoError(t, os.WriteFile(configPath, []byte(changed), 0o600))
			reload := exec.CommandContext(t.Context(), binary, "daemon", "reload") //nolint:gosec // isolated test-built CLI
			reload.Env = env
			output, err := reload.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Eventually(t, func() bool {
				return strings.Contains(readLog(), "AgentsView target or enabled state changed; restart required")
			}, 10*time.Second, 50*time.Millisecond) //nolint:kennlint // asynchronous daemon reload log
		})
	}
}

func TestAgentsViewInvalidConfigStopsDaemonStartup(t *testing.T) {
	_ = daemonLifecycleEnvironment(t)
	home := setupKataEnv(t)
	t.Setenv("KATA_WORKSPACE", t.TempDir())
	// Resolve owns the credential cases; startup only proves each selector kind
	// reaches it and stops the daemon without echoing the selector.
	for _, tc := range []struct{ name, kind, selector string }{
		{"missing file", "token_file", filepath.Join(home, "fixture-selector-secret-missing")},
		{"missing env", "token_env", "FIXTURE_SELECTOR_SECRET_MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configText := "[agentsview]\nurl = 'https://agentsview.example'\n" + tc.kind + " = '" + tc.selector + "'\n"
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600))
			_, err := preflightDaemonStartup(t.Context(), "127.0.0.1:0", false, true)
			require.ErrorContains(t, err, "[agentsview]")
			require.ErrorContains(t, err, tc.kind)
			require.ErrorIs(t, err, config.ErrCredentialSource)
			require.NotContains(t, err.Error(), tc.selector)
		})
	}
}

func assertAgentsViewInvalidCredentialReloadKeepsClient(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	headers := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"session_not_found","message":"not found"}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("FIXTURE_SELECTOR_SECRET", "good-token")
	cfg := config.AgentsViewConfig{URL: upstream.URL, TokenEnv: "FIXTURE_SELECTOR_SECRET"}
	resolved, err := cfg.Resolve(t.Context(), false)
	require.NoError(t, err)
	service, err := agentsview.NewService(resolved, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[agentsview]\nurl = '"+upstream.URL+"'\ntoken_env = 'FIXTURE_SELECTOR_SECRET'\n"), 0600))
	t.Setenv("FIXTURE_SELECTOR_SECRET", "bad\r\ntoken")
	_, err = reloadAgentsView(t.Context(), home, service, false)
	require.ErrorContains(t, err, "credential")
	require.ErrorContains(t, err, "token_env")
	require.ErrorIs(t, err, config.ErrCredentialSource)
	require.NotContains(t, err.Error(), "FIXTURE_SELECTOR_SECRET")
	require.NotContains(t, err.Error(), "bad")
	sessions := []cron.Session{{Agent: "example", SessionID: "id"}}
	result, err := service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
	require.NoError(t, err)
	require.Equal(t, "not_found", result.Sessions[0].State)
	require.Equal(t, "Bearer good-token", <-headers)
}

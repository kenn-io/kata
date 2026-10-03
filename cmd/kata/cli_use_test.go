package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/testenv"
)

const cliAppOpenedBody = `{"event":"app_opened","properties":{"surface":"cli"}}`

// cliUseCapture records every body the daemon forwards and answers with status.
type cliUseCapture struct {
	status string
	mu     sync.Mutex
	bodies []string
}

func (c *cliUseCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, string(bytes.TrimSpace(body)))
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"` + c.status + `"}`))
}

func (c *cliUseCapture) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// newCLIUseEnv boots a daemon whose capture handler is capture, with no hook marker inherited.
func newCLIUseEnv(t *testing.T, capture http.Handler) *testenv.Env {
	t.Helper()
	t.Setenv(hooks.HookVersionEnv, "")
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	return testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.TelemetryCapture = capture })
}

func shortenCLIUseReportTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	saved := cliUseReportTimeout
	cliUseReportTimeout = timeout
	t.Cleanup(func() { cliUseReportTimeout = saved })
}

// findCommand returns the command a fresh root would run for path, with flags reset.
func findCommand(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	resetFlags(t)
	cmd, _, err := newRootCmd().Find(path)
	require.NoError(t, err)
	return cmd
}

func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	return addr
}

func TestCLIUseReportsOnceThroughDaemon(t *testing.T) {
	baseline, err := runCmdOutput(t, newCLIUseEnv(t, nil), "projects", "list")
	require.NoError(t, err)
	capture := &cliUseCapture{status: "queued"}
	env := newCLIUseEnv(t, capture)

	first, err := runCmdOutput(t, env, "projects", "list")
	require.NoError(t, err)
	target := cliUseTarget.Load()
	require.NotNil(t, target, "the command's own resolution records its target")
	assert.Equal(t, env.URL, target.BaseURL)
	assert.Equal(t, client.DaemonSourceInjected, target.Source)
	second, err := runCmdOutput(t, env, "projects", "list")
	require.NoError(t, err)

	assert.Equal(t, baseline, first)
	assert.Equal(t, baseline, second)
	bodies := capture.recorded()
	require.Len(t, bodies, 1)
	assert.JSONEq(t, cliAppOpenedBody, bodies[0])
}

func TestCLIUseSkipsAgentCallers(t *testing.T) {
	// A capture answering disabled is never marked by the daemon gate, so every post reaches it.
	t.Run("agent output modes and hook children", func(t *testing.T) {
		for _, test := range []struct {
			name string
			hook string
			args []string
		}{
			{name: "agent flag", args: []string{"projects", "list", "--agent"}},
			{name: "agent format", args: []string{"projects", "list", "--format", "agent"}},
			{name: "daemon hook child", hook: "1", args: []string{"projects", "list"}},
		} {
			t.Run(test.name, func(t *testing.T) {
				capture := &cliUseCapture{status: "disabled"}
				env := newCLIUseEnv(t, capture)
				t.Setenv(hooks.HookVersionEnv, test.hook)

				_, err := runCmdOutput(t, env, test.args...)

				require.NoError(t, err)
				require.NotNil(t, cliUseTarget.Load(), "the command resolved a daemon")
				assert.Empty(t, capture.recorded())
			})
		}
	})

	t.Run("attention hook", func(t *testing.T) {
		capture := &cliUseCapture{status: "disabled"}
		env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.TelemetryCapture = capture })
		t.Setenv(hooks.HookVersionEnv, "")
		t.Cleanup(func() { cliUseTarget.Store(nil) })
		t.Setenv("KATA_REF", createIssue(t, env, pid, "launcher-tracked work"))

		require.NoError(t, runAttnHook(t, env, dir, "attention-hook", "start"))

		require.NotNil(t, cliUseTarget.Load(), "the hook resolved a daemon")
		assert.Empty(t, capture.recorded())
	})

	t.Run("command classification", func(t *testing.T) {
		t.Setenv(hooks.HookVersionEnv, "")
		for _, test := range []struct {
			path []string
			want bool
		}{
			{path: []string{"mcp", "serve"}, want: false},
			{path: []string{"agent-hooks", "attention", "start"}, want: false},
			{path: []string{"agent-hooks", "contract"}, want: false},
			{path: []string{"agent-contract-hook"}, want: false},
			{path: []string{"projects", "list"}, want: true},
			{path: []string{"create"}, want: true},
			{path: []string{"mcp", "status"}, want: true},
		} {
			t.Run(strings.Join(test.path, " "), func(t *testing.T) {
				assert.Equal(t, test.want, reportsCLIUse(findCommand(t, test.path...)))
			})
		}
	})
}

func TestCLIUseNeverStartsDaemon(t *testing.T) {
	t.Run("command against an unreachable daemon", func(t *testing.T) {
		t.Setenv("KATA_HOME", t.TempDir())
		t.Setenv("KATA_AUTH_TOKEN", "")
		t.Setenv(hooks.HookVersionEnv, "")
		t.Setenv("KATA_SERVER", "http://"+closedLoopbackAddr(t))

		_, err := runCmdOutput(t, nil, "projects", "list")

		require.Error(t, err)
		assert.Equal(t, ExitDaemonUnavail, exitCodeForErr(err, true))
		t.Setenv("KATA_SERVER", "")
		_, err = discoverDaemon(context.Background())
		var ce *cliError
		require.ErrorAs(t, err, &ce)
		assert.Contains(t, ce.Message, "no daemon running")
	})

	t.Run("report against an unreachable target", func(t *testing.T) {
		t.Setenv("KATA_HOME", t.TempDir())
		t.Setenv("KATA_SERVER", "")
		t.Setenv(hooks.HookVersionEnv, "")
		shortenCLIUseReportTimeout(t, 200*time.Millisecond)
		cmd := findCommand(t, "projects", "list")
		cliUseTarget.Store(&client.ResolvedDaemon{BaseURL: "http://" + closedLoopbackAddr(t)})
		t.Cleanup(func() { cliUseTarget.Store(nil) })
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetContext(context.Background())

		start := time.Now()
		reportCLIUse(cmd)

		assert.Less(t, time.Since(start), cliUseReportTimeout+time.Second)
		assert.Empty(t, stdout.String())
		assert.Empty(t, stderr.String())
		_, err := discoverDaemon(context.Background())
		var ce *cliError
		require.ErrorAs(t, err, &ce)
		assert.Contains(t, ce.Message, "no daemon running")
	})
}

func TestCLIUseFailureIsSilent(t *testing.T) {
	shortenCLIUseReportTimeout(t, 200*time.Millisecond)
	baseline, err := runCmdOutput(t, newCLIUseEnv(t, nil), "projects", "list")
	require.NoError(t, err)

	for _, test := range []struct {
		name    string
		capture http.Handler
	}{
		{name: "capture fails", capture: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})},
		{name: "capture hangs", capture: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newCLIUseEnv(t, test.capture)

			start := time.Now()
			stdout, stderr, err := runCmdCapture(t, env, "projects", "list")

			require.NoError(t, err)
			assert.Equal(t, baseline, stdout)
			assert.Empty(t, stderr)
			assert.Less(t, time.Since(start), cliUseReportTimeout+2*time.Second)
		})
	}
}

func TestCLIUseSkipsProbes(t *testing.T) {
	capture := &cliUseCapture{status: "disabled"}
	env := newCLIUseEnv(t, capture)

	_, err := runCmdOutput(t, env, "health")

	require.NoError(t, err)
	assert.Nil(t, cliUseTarget.Load())
	assert.Empty(t, capture.recorded())
}

func TestCLIUseReportsAfterCanceledContext(t *testing.T) {
	capture := &cliUseCapture{status: "queued"}
	env := newCLIUseEnv(t, capture)
	cmd := findCommand(t, "events")
	cliUseTarget.Store(&client.ResolvedDaemon{BaseURL: env.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)

	reportCLIUse(cmd)

	bodies := capture.recorded()
	require.Len(t, bodies, 1)
	assert.JSONEq(t, cliAppOpenedBody, bodies[0])
}

func TestCLIUseReportsOverUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-socket transport is not selected on Windows")
	}
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv(hooks.HookVersionEnv, "")
	type request struct{ method, path, body string }
	var mu sync.Mutex
	var requests []request
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, request{r.Method, r.URL.Path, string(bytes.TrimSpace(body))})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	})
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	cmd := findCommand(t, "projects", "list")
	cliUseTarget.Store(&client.ResolvedDaemon{BaseURL: client.UnixBase, UnixSocket: socketPath})
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	cmd.SetContext(context.Background())

	reportCLIUse(cmd)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 1)
	assert.Equal(t, http.MethodPost, requests[0].method)
	assert.Equal(t, "/api/v1/ui/telemetry", requests[0].path)
	assert.JSONEq(t, cliAppOpenedBody, requests[0].body)
}

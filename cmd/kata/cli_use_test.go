package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// cliUseTelemetry is the daemon's reporter; block, when set, holds every capture until closed.
type cliUseTelemetry struct {
	block    chan struct{}
	mu       sync.Mutex
	captured []map[string]any
}

func (*cliUseTelemetry) EventAllowed(event string) bool { return event == "app_opened" }
func (*cliUseTelemetry) Enabled() bool                  { return true }
func (c *cliUseTelemetry) Capture(_ string, properties map[string]any) error {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, properties)
	return nil
}

func (c *cliUseTelemetry) recorded() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.captured...)
}

var cliSurface = []map[string]any{{"surface": "cli"}}

// newCLIUseEnv boots a daemon whose reporter is reporter, for a person at a
// terminal with no hook marker inherited.
func newCLIUseEnv(t *testing.T, reporter daemon.TelemetryReporter) *testenv.Env {
	t.Helper()
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	return testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = reporter })
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
	capture := &cliUseTelemetry{}
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
	assert.Equal(t, cliSurface, capture.recorded(), "the daemon forwards one cli open per day")
}

func TestCLIUseSkipsAgentCallers(t *testing.T) {
	t.Run("agent output modes and hook children", func(t *testing.T) {
		for _, test := range []struct {
			name string
			hook string
			args []string
		}{
			{name: "agent flag", args: []string{"projects", "list", "--agent"}},
			{name: "agent format", args: []string{"projects", "list", "--format", "agent"}},
			{name: "daemon hook child", hook: "1", args: []string{"projects", "list"}},
			{name: "harness context", args: []string{"inbox", "--context", "--all", "--for", "actor/teammate"}},
		} {
			t.Run(test.name, func(t *testing.T) {
				capture := &cliUseTelemetry{}
				env := newCLIUseEnv(t, capture)
				t.Setenv(hooks.HookVersionEnv, test.hook)

				_, err := runCmdOutput(t, env, test.args...)

				require.NoError(t, err)
				require.NotNil(t, cliUseTarget.Load(), "the command resolved a daemon")
				assert.Empty(t, capture.recorded())
			})
		}
	})

	for _, args := range [][]string{{"attention-hook", "start"}, {"agent-hook", "attention", "start"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			capture := &cliUseTelemetry{}
			env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = capture })
			t.Setenv(hooks.HookVersionEnv, "")
			t.Cleanup(func() { cliUseTarget.Store(nil) })
			t.Setenv("KATA_REF", createIssue(t, env, pid, "launcher-tracked work"))

			require.NoError(t, runAttnHook(t, env, dir, args...))

			require.NotNil(t, cliUseTarget.Load(), "the hook resolved a daemon")
			assert.Empty(t, capture.recorded())
		})
	}

	t.Run("command classification", func(t *testing.T) {
		t.Setenv(hooks.HookVersionEnv, "")
		stubIsTTY(t, true)
		for _, test := range []struct {
			path []string
			want bool
		}{
			{path: []string{"mcp", "serve"}, want: false},
			{path: []string{"agent-hook", "attention", "start"}, want: false},
			{path: []string{"agent-hook", "contract"}, want: false},
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
	hung := &cliUseTelemetry{block: make(chan struct{})}
	env := newCLIUseEnv(t, hung)
	t.Cleanup(func() { close(hung.block) }) // before the daemon shuts down, so shutdown doesn't wait on it

	start := time.Now()
	stdout, stderr, err := runCmdCapture(t, env, "projects", "list")

	require.NoError(t, err)
	assert.Equal(t, baseline, stdout)
	assert.Empty(t, stderr)
	assert.Less(t, time.Since(start), cliUseReportTimeout+2*time.Second)
}

func TestCLIUseSkipsProbes(t *testing.T) {
	capture := &cliUseTelemetry{}
	env := newCLIUseEnv(t, capture)

	_, err := runCmdOutput(t, env, "health")

	require.NoError(t, err)
	assert.Nil(t, cliUseTarget.Load())
	assert.Empty(t, capture.recorded())
}

func TestCLIUseReportsAfterCanceledContext(t *testing.T) {
	capture := &cliUseTelemetry{}
	env := newCLIUseEnv(t, capture)
	cmd := findCommand(t, "events")
	cliUseTarget.Store(&client.ResolvedDaemon{BaseURL: env.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)

	reportCLIUse(cmd)

	assert.Equal(t, cliSurface, capture.recorded())
}

// cliUseDaemon is a daemon stand-in that counts app_opened posts and answers with status.
type cliUseDaemon struct {
	url    string
	status atomic.Int32
	posts  atomic.Int32
}

func newCLIUseDaemon(t *testing.T) *cliUseDaemon {
	t.Helper()
	d := &cliUseDaemon{}
	d.status.Store(http.StatusAccepted)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/ui/telemetry" {
			http.NotFound(w, r)
			return
		}
		d.posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(d.status.Load()))
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	t.Cleanup(server.Close)
	d.url = server.URL
	return d
}

func setCLIUseClock(t *testing.T, now time.Time) {
	t.Helper()
	saved := cliUseNow
	cliUseNow = func() time.Time { return now }
	t.Cleanup(func() { cliUseNow = saved })
}

// reportCLIUseTo runs the use report for a typed command that used the daemon at url.
func reportCLIUseTo(t *testing.T, url string) {
	t.Helper()
	cmd := findCommand(t, "projects", "list")
	cmd.SetContext(context.Background())
	cliUseTarget.Store(&client.ResolvedDaemon{BaseURL: url})
	t.Cleanup(func() { cliUseTarget.Store(nil) })
	reportCLIUse(cmd)
}

func TestCLIUseReportsOncePerDayPerDaemon(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	first, second := newCLIUseDaemon(t), newCLIUseDaemon(t)
	setCLIUseClock(t, time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC))

	reportCLIUseTo(t, first.url)
	reportCLIUseTo(t, first.url)
	reportCLIUseTo(t, second.url)
	assert.Equal(t, int32(1), first.posts.Load(), "a later command the same day sends nothing")
	assert.Equal(t, int32(1), second.posts.Load(), "each daemon counts its own day")

	setCLIUseClock(t, time.Date(2026, 10, 3, 0, 1, 0, 0, time.UTC))
	reportCLIUseTo(t, first.url)
	assert.Equal(t, int32(2), first.posts.Load(), "the next UTC day reports again")
}

func TestCLIUseRetriesUntilTheDaemonAccepts(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	target := newCLIUseDaemon(t)
	target.status.Store(http.StatusServiceUnavailable)

	reportCLIUseTo(t, target.url)
	target.status.Store(http.StatusAccepted)
	reportCLIUseTo(t, target.url)
	reportCLIUseTo(t, target.url)

	assert.Equal(t, int32(2), target.posts.Load(), "only an accepted report ends the day's reporting")
}

func TestCLIUseSkipsOutputThatIsNotATerminal(t *testing.T) {
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, false)

	assert.False(t, reportsCLIUse(findCommand(t, "projects", "list")),
		"agents and scripts read output through a pipe")
}

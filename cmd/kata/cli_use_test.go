package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/tui"
	kataclient "go.kenn.io/kata/pkg/client"
)

// cliUseTelemetry is the daemon's reporter; block holds use reports until closed.
type cliUseTelemetry struct {
	block    chan struct{}
	started  chan struct{}
	mu       sync.Mutex
	captured []map[string]any
	events   []string
}

func (*cliUseTelemetry) EventAllowed(event string) bool {
	return event == "app_opened" || event == "agent_active" || event == "agent_call_count"
}
func (*cliUseTelemetry) Enabled() bool { return true }
func (*cliUseTelemetry) SanitizeProperties(string, map[string]any) (map[string]any, error) {
	return nil, telemetry.ErrUnsupportedEvent
}
func (c *cliUseTelemetry) Capture(event string, properties map[string]any) error {
	if c.block != nil && (event == "app_opened" || event == "agent_active") {
		if c.started != nil {
			c.started <- struct{}{}
		}
		<-c.block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, properties)
	c.events = append(c.events, event)
	return nil
}

func (c *cliUseTelemetry) recorded() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.captured...)
}

func (c *cliUseTelemetry) waitForCapture(t *testing.T) {
	t.Helper()
	//nolint:kennlint // Real daemon delivery can finish after the command's exit wait.
	require.Eventually(t, func() bool { return len(c.recorded()) > 0 }, 5*time.Second, time.Millisecond)
}

func waitCLIUseMarker(t *testing.T, prefix string) {
	t.Helper()
	target := cliUseTarget.Load()
	require.NotNil(t, target)
	marker := client.TelemetryDayMarkerPath(*target, prefix)
	day := cliUseNow().UTC().Format(time.DateOnly)
	//nolint:kennlint // The daily marker signals that the real HTTP response was handled.
	require.Eventually(t, func() bool { return client.ReadTelemetryDayMarker(marker) == day }, 5*time.Second, time.Millisecond)
}

// settledCLIUse waits for command-owned report workers before negative assertions.
func settledCLIUse(t *testing.T, run func(*testing.T)) {
	t.Helper()
	saved := http.DefaultTransport
	transport := saved.(*http.Transport).Clone()
	transport.DisableKeepAlives = true // Idle real-network readers would keep the synctest bubble alive.
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = saved }()
	synctest.Test(t, func(t *testing.T) {
		run(t)
		synctest.Wait()
	})
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
	waitCLIUseMarker(t, "cli-opened-")
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

				settledCLIUse(t, func(t *testing.T) {
					_, err := runCmdOutput(t, env, test.args...)
					require.NoError(t, err)
				})
				require.NotNil(t, cliUseTarget.Load(), "the command resolved a daemon")
				if test.hook != "" {
					capture.waitForCapture(t)
					assert.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}}, capture.recorded())
				} else {
					assert.Empty(t, capture.recorded())
				}
			})
		}
	})

	for _, args := range [][]string{{"attention-hook", "start"}, {"agent-hook", "attention", "start"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			capture := &cliUseTelemetry{}
			env, dir, pid := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = capture })
			t.Setenv(hooks.HookVersionEnv, "")
			t.Cleanup(func() { cliUseTarget.Store(nil) })
			if args[0] == "attention-hook" {
				t.Setenv("KATA_SERVER", env.URL)
				t.Setenv("KATA_REF", "")
				t.Setenv("CLAUDE_PROJECT_DIR", dir)
				for _, args := range [][]string{
					{"attention-hook", "--help"},
					{"attention-hook", "foo"},
					{"attention-hook", "start"},
					{"agent-hook", "attention", "start"},
				} {
					settledCLIUse(t, func(t *testing.T) {
						_, _, err := runCmdCapture(t, env, args...)
						require.NoError(t, err)
					})
					require.Nil(t, cliUseTarget.Load(), args)
					require.Empty(t, capture.recorded(), args)
				}
				settledCLIUse(t, func(t *testing.T) {
					_, _, err := executeAgentHook(t, strings.NewReader(`{}`), "agent-hook", "attention-native", "claude", "start")
					require.NoError(t, err)
				})
				require.Nil(t, cliUseTarget.Load())
				require.Empty(t, capture.recorded())
			}
			t.Setenv("KATA_REF", createIssue(t, env, pid, "launcher-tracked work"))

			require.NoError(t, runAttnHook(t, env, dir, args...))

			require.NotNil(t, cliUseTarget.Load(), "the hook resolved a daemon")
			capture.waitForCapture(t)
			assert.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}}, capture.recorded())
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
		stubIsTTY(t, true)
		cmd := findCommand(t, "projects", "list")
		cliUseTarget.Store(&client.ResolvedDaemon{BaseURL: "http://" + closedLoopbackAddr(t)})
		t.Cleanup(func() { cliUseTarget.Store(nil) })
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetContext(context.Background())

		settledCLIUse(t, func(*testing.T) { reportCLIUse(cmd) })

		assert.Empty(t, stdout.String())
		assert.Empty(t, stderr.String())
		_, err := discoverDaemon(context.Background())
		var ce *cliError
		require.ErrorAs(t, err, &ce)
		assert.Contains(t, ce.Message, "no daemon running")
	})
	t.Run("contract hook discovery", func(t *testing.T) {
		t.Setenv("KATA_HOME", t.TempDir())
		t.Setenv("KATA_SERVER", "http://"+closedLoopbackAddr(t))
		t.Setenv("KATA_AUTH_TOKEN", "")
		output, stderr, err := runCmdCapture(t, nil, "agent-contract-hook")
		require.NoError(t, err)
		require.Contains(t, output, "SessionStart")
		require.Empty(t, stderr)
		t.Setenv("KATA_SERVER", "")
		_, err = discoverDaemon(context.Background())
		require.ErrorContains(t, err, "no daemon running")
	})
}

func TestCLIUseSkipsProbes(t *testing.T) {
	capture := &cliUseTelemetry{}
	env := newCLIUseEnv(t, capture)
	t.Setenv("KATA_SERVER", env.URL)
	for _, test := range []struct {
		hook string
		args []string
	}{
		{"", []string{"health"}},
		{"1", []string{"version"}},
		{"1", []string{"projects", "list", "--help"}},
	} {
		t.Setenv(hooks.HookVersionEnv, test.hook)
		cliUseTarget.Store(nil)
		settledCLIUse(t, func(t *testing.T) {
			_, err := runCmdOutput(t, env, test.args...)
			require.NoError(t, err)
		})
		assert.Nil(t, cliUseTarget.Load())
		assert.Empty(t, capture.recorded())
	}
}

func TestCLIUseStalledReportPreservesCreate(t *testing.T) {
	hung := &cliUseTelemetry{block: make(chan struct{}), started: make(chan struct{}, 1)}
	env, dir, _ := setupCLIWorkspaceOptions(t, func(cfg *daemon.ServerConfig) { cfg.Telemetry = hung })
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	release := sync.OnceFunc(func() { close(hung.block) })
	t.Cleanup(release)
	type commandResult struct {
		stdout, stderr string
		err            error
	}
	done := make(chan commandResult, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		stdout, stderr, err := runCLIWithErr(t, env, dir, "create", "exit wait example")
		done <- commandResult{stdout: stdout, stderr: stderr, err: err}
	}()
	t.Cleanup(func() {
		release()
		<-joined
	})
	select {
	case <-hung.started:
	case <-time.After(10 * time.Second):
		t.Fatal("create did not reach the held telemetry handler")
	}
	watchdog := time.NewTimer(10 * time.Second) //nolint:kennlint // Bound the real command liveness check; synctest owns the exact exit cap.
	defer watchdog.Stop()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.Contains(t, result.stdout, "exit wait example")
		assert.Empty(t, result.stderr)
	case <-watchdog.C:
		t.Fatal("create waited for the held telemetry response")
	}
	release()
	attempt := client.TelemetryDayMarkerPath(*cliUseTarget.Load(), "cli-opened-try-")
	//nolint:kennlint // Join the send worker before removing its fixture, regardless of its outcome.
	require.Eventually(t, func() bool { return client.ReadTelemetryDayMarker(attempt) == "" }, 5*time.Second, time.Millisecond)
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
	waitCLIUseMarker(t, "cli-opened-")

	assert.Equal(t, cliSurface, capture.recorded())
}

// cliUseDaemon counts telemetry posts and returns the configured status and receipt.
type cliUseDaemon struct {
	url     string
	status  atomic.Int32
	posts   atomic.Int32
	receipt atomic.Value
}

func newCLIUseDaemon(t *testing.T) *cliUseDaemon {
	t.Helper()
	d := &cliUseDaemon{}
	d.status.Store(http.StatusAccepted)
	d.receipt.Store("queued")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/ui/telemetry" {
			http.NotFound(w, r)
			return
		}
		status, receipt := int(d.status.Load()), d.receipt.Load()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"status":%q}`, receipt)
		d.posts.Add(1)
	}))
	server.Config.SetKeepAlivesEnabled(false)
	t.Cleanup(server.Close)
	d.url = server.URL
	return d
}

func (d *cliUseDaemon) waitForPosts(t *testing.T, want int32) {
	t.Helper()
	//nolint:kennlint // Wait for real HTTP handlers before asserting counts or changing their response.
	require.Eventually(t, func() bool { return d.posts.Load() == want }, 5*time.Second, time.Millisecond)
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
	waitCLIUseMarker(t, "cli-opened-")
	reportCLIUseTo(t, first.url)
	reportCLIUseTo(t, second.url)
	waitCLIUseMarker(t, "cli-opened-")
	assert.Equal(t, int32(1), first.posts.Load(), "a later command the same day sends nothing")
	assert.Equal(t, int32(1), second.posts.Load(), "each daemon counts its own day")

	setCLIUseClock(t, time.Date(2026, 10, 3, 0, 1, 0, 0, time.UTC))
	reportCLIUseTo(t, first.url)
	waitCLIUseMarker(t, "cli-opened-")
	assert.Equal(t, int32(2), first.posts.Load(), "the next UTC day reports again")
}

func TestCLIUseRetriesUntilTheDaemonAccepts(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	target := newCLIUseDaemon(t)
	target.status.Store(http.StatusServiceUnavailable)

	reportCLIUseTo(t, target.url)
	target.waitForPosts(t, 1)
	attempt := client.TelemetryDayMarkerPath(*cliUseTarget.Load(), "cli-opened-try-")
	//nolint:kennlint // Observe the handled failure before invoking the next command.
	require.Eventually(t, func() bool { return client.ReadTelemetryDayMarker(attempt) == "" }, 5*time.Second, time.Millisecond)
	target.status.Store(http.StatusAccepted)
	reportCLIUseTo(t, target.url)
	waitCLIUseMarker(t, "cli-opened-")
	reportCLIUseTo(t, target.url)

	assert.Equal(t, int32(2), target.posts.Load(), "only an accepted report ends the day's reporting")
}

func TestCLIUseBacksOffUnobservedReports(t *testing.T) {
	for _, test := range []struct{ first, second, retry string }{
		{"2026-10-02T12:00:00Z", "2026-10-02T12:01:00Z", "2026-10-02T12:16:00Z"},
		{"2026-10-02T23:59:00Z", "2026-10-02T23:59:30Z", "2026-10-03T00:00:00Z"},
	} {
		t.Run(test.retry, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			held, started := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(held) })
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/ui/telemetry", r.URL.Path)
				if posts.Add(1) == 1 {
					close(started)
					<-held
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"queued"}`))
			}))
			t.Cleanup(server.Close)
			t.Cleanup(release)
			run := func(now, mode string) *exec.Cmd {
				process := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCLIUseBackoffProcess$") //nolint:gosec // Current test executable with fixed fixture arguments.
				process.Env = append(os.Environ(), "KATA_CLI_USE_TEST_SERVER="+server.URL, "KATA_CLI_USE_TEST_NOW="+now, "KATA_CLI_USE_TEST_MODE="+mode, "GORACE=atexit_sleep_ms=0")
				return process
			}
			first := run(test.first, "exit")
			var output bytes.Buffer
			first.Stdout, first.Stderr = &output, &output
			require.NoError(t, first.Start())
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				release()
				_ = first.Wait()
				t.Fatal("first process did not reach the held telemetry response")
			}
			require.NoError(t, first.Wait(), "%s", output.String())
			release()

			outputBytes, err := run(test.second, "exit").CombinedOutput()
			require.NoError(t, err, "%s", outputBytes)
			require.EqualValues(t, 1, posts.Load(), "a new process skips a report whose unseen outcome is still in backoff")
			attempt := client.TelemetryDayMarkerPath(client.ResolvedDaemon{BaseURL: server.URL}, "cli-opened-try-")
			require.Equal(t, test.first, client.ReadTelemetryDayMarker(attempt), "a skipped report leaves the unseen attempt in place")

			outputBytes, err = run(test.retry, "accepted").CombinedOutput()
			require.NoError(t, err, "%s", outputBytes)
			require.EqualValues(t, 2, posts.Load(), "backoff expiry or a new UTC day permits a retry")
		})
	}
}

func TestCLIUseBackoffProcess(t *testing.T) {
	url := os.Getenv("KATA_CLI_USE_TEST_SERVER")
	if url == "" {
		return
	}
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, true)
	now, err := time.Parse(time.RFC3339, os.Getenv("KATA_CLI_USE_TEST_NOW"))
	require.NoError(t, err)
	setCLIUseClock(t, now)
	reportCLIUseTo(t, url)
	if os.Getenv("KATA_CLI_USE_TEST_MODE") == "accepted" {
		waitCLIUseMarker(t, "cli-opened-")
	}
}

func TestCLIUseBoundsExitWait(t *testing.T) {
	for _, hook := range []string{"", "1"} {
		t.Run("hook="+hook, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			t.Setenv("KATA_AUTH_TOKEN", "")
			t.Setenv(hooks.HookVersionEnv, hook)
			stubIsTTY(t, true)
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "/api/v1/ui/telemetry", r.URL.Path)
					<-release
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"status":"queued"}`))
				}))
				saved := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				defer func() { http.DefaultTransport = saved }()
				start := time.Now()
				reportCLIUseTo(t, "http://127.0.0.1:7777")
				assert.Equal(t, 50*time.Millisecond, time.Since(start))
				close(release)
				synctest.Wait()
			})
		})
	}
}

func TestCLIUseSkipsOutputThatIsNotATerminal(t *testing.T) {
	t.Setenv(hooks.HookVersionEnv, "")
	stubIsTTY(t, false)

	assert.False(t, reportsCLIUse(findCommand(t, "projects", "list")),
		"agents and scripts read output through a pipe")
}

func TestAgentContractUsePreservesOutput(t *testing.T) {
	baseline, err := runCmdOutput(t, newCLIUseEnv(t, nil), "agent-contract-hook")
	require.NoError(t, err)
	capture := &cliUseTelemetry{}
	env := newCLIUseEnv(t, capture)
	output, stderr, err := runCmdCapture(t, env, "agent-contract-hook")
	require.NoError(t, err)
	require.Equal(t, baseline, output)
	require.Empty(t, stderr)
	capture.waitForCapture(t)
	require.Equal(t, []map[string]any{{"call_count_bucket": "1-10"}}, capture.recorded())
}

func TestAgentUseFullDay(t *testing.T) {
	for _, test := range []struct {
		name, receipt string
		want          int32
	}{
		{"full", "full", 1},
		{"telemetry disabled", "disabled", 1},
		{"older daemon queued", "queued", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			t.Setenv("KATA_AUTH_TOKEN", "")
			t.Setenv(hooks.HookVersionEnv, "1")
			setCLIUseClock(t, time.Date(2026, 10, 2, 23, 59, 0, 0, time.UTC))
			target := newCLIUseDaemon(t)
			target.receipt.Store(test.receipt)
			reportCLIUseTo(t, target.url)
			target.waitForPosts(t, 1)
			if test.want == 1 {
				waitCLIUseMarker(t, "agent-full-")
			}
			reportCLIUseTo(t, target.url)
			if test.want == 2 {
				target.waitForPosts(t, 2)
			}
			require.Equal(t, test.want, target.posts.Load())
			if test.want == 1 {
				// A fresh hook process must reuse the target's marker.
				process := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAgentUseFullDayProcess$") //nolint:gosec // Current test executable with fixed fixture arguments.
				process.Env = append(os.Environ(), "KATA_AGENT_USE_TEST_SERVER="+target.url)
				output, err := process.CombinedOutput()
				require.NoError(t, err, "%s", output)
				require.EqualValues(t, 1, target.posts.Load())
			}
			setCLIUseClock(t, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
			reportCLIUseTo(t, target.url)
			target.waitForPosts(t, test.want+1)
			if test.want == 1 {
				waitCLIUseMarker(t, "agent-full-")
			}
			require.Equal(t, test.want+1, target.posts.Load(), "UTC rollover resumes reporting")
		})
	}
}

func TestAgentUseFullDayProcess(t *testing.T) {
	url := os.Getenv("KATA_AGENT_USE_TEST_SERVER")
	if url == "" {
		return
	}
	agentUseReportTimeout = 5 * time.Second
	setCLIUseClock(t, time.Date(2026, 10, 2, 23, 59, 0, 0, time.UTC))
	if os.Getenv("KATA_AGENT_USE_TEST_DISCOVER") == "1" {
		cmd := findCommand(t, "agent-contract-hook")
		flags.Daemon = os.Getenv("KATA_AGENT_USE_TEST_DAEMON")
		if os.Getenv("KATA_AGENT_USE_TEST_INJECT") == "1" {
			cmd.SetContext(context.WithValue(t.Context(), client.BaseURLKey{}, url))
		}
		reportCLIUse(cmd)
		return
	}
	reportCLIUseTo(t, url)
}

func TestDisabledTelemetrySharedAcrossTerminalCallers(t *testing.T) {
	for _, first := range []string{"cli", "hook", "tui", "mcp"} {
		t.Run(first, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			t.Setenv("KATA_AUTH_TOKEN", "")
			stubIsTTY(t, true)
			target := newCLIUseDaemon(t)
			target.receipt.Store("disabled")
			synctest.Test(t, func(t *testing.T) {
				report := func(caller string) {
					switch caller {
					case "mcp":
						c, err := kataclient.NewWithHTTPClient(target.url, http.DefaultClient)
						require.NoError(t, err)
						observe, stop := startMCPAgentUseReporter(t.Context(), c, client.ResolvedDaemon{BaseURL: target.url})
						observe()
						synctest.Wait()
						stop()
					case "tui":
						require.NoError(t, tui.NewClient(target.url, http.DefaultClient).ReportScreenViewed(t.Context(), "issues"))
					default:
						t.Setenv(hooks.HookVersionEnv, "")
						if caller == "hook" {
							t.Setenv(hooks.HookVersionEnv, "1")
						}
						reportCLIUseTo(t, target.url)
					}
				}
				report(first)
				for _, caller := range []string{"cli", "hook", "tui", "mcp"} {
					report(caller)
				}
				require.Equal(t, int32(1), target.posts.Load())
			})
		})
	}
}

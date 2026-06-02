package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonStatus_NoDaemonReportsAbsent(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newDaemonCmd(), "status")
	assert.Contains(t, string(out), "no daemon")
}

func TestDaemonStatus_JSONReportsDaemonsWithVersion(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	started := time.Date(2026, 5, 4, 1, 2, 3, 0, time.UTC)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   "unix",
		Address:   "/tmp/kata-test.sock",
		Metadata:  map[string]string{"db_path": filepath.Join(tmp, "kata.db")},
		Version:   "v-test-status",
		StartedAt: started,
	})
	require.NoError(t, err)

	out := executeRoot(t, newRootCmd(), "daemon", "status", "--json")

	var got struct {
		KataAPIVersion int `json:"kata_api_version"`
		Daemons        []struct {
			PID       int    `json:"pid"`
			Version   string `json:"version"`
			Address   string `json:"address"`
			DBPath    string `json:"db_path"`
			StartedAt string `json:"started_at"`
		} `json:"daemons"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, 1, got.KataAPIVersion)
	require.Len(t, got.Daemons, 1)
	assert.Equal(t, os.Getpid(), got.Daemons[0].PID)
	assert.Equal(t, "v-test-status", got.Daemons[0].Version)
	assert.Equal(t, "unix:///tmp/kata-test.sock", got.Daemons[0].Address)
	assert.Equal(t, filepath.Join(tmp, "kata.db"), got.Daemons[0].DBPath)
	assert.Equal(t, started.Format(time.RFC3339), got.Daemons[0].StartedAt)
}

func TestDaemonStatus_JSONReportsDBPathFromKitRuntimeMetadata(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	started := time.Date(2026, 5, 4, 1, 2, 3, 0, time.UTC)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   "unix",
		Address:   "/tmp/kata-test.sock",
		Service:   "kata",
		Version:   "v-test-status",
		StartedAt: started,
		Metadata: map[string]string{
			"db_path": filepath.Join(tmp, "kata.db"),
		},
	})
	require.NoError(t, err)

	out := executeRoot(t, newRootCmd(), "daemon", "status", "--json")

	var got struct {
		Daemons []struct {
			DBPath string `json:"db_path"`
		} `json:"daemons"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	require.Len(t, got.Daemons, 1)
	assert.Equal(t, filepath.Join(tmp, "kata.db"), got.Daemons[0].DBPath)
}

func TestDaemonStatus_JSONReportsEmptyDaemonList(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newRootCmd(), "daemon", "status", "--json")

	var got struct {
		KataAPIVersion int             `json:"kata_api_version"`
		Daemons        json.RawMessage `json:"daemons"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.JSONEq(t, "[]", string(got.Daemons))
}

func TestDaemonStatus_AgentReportsStopped(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "status")
	assert.Equal(t, "OK daemon status=stopped\n", string(out))
}

func TestRuntimeRecordRedactsPostgresDSN(t *testing.T) {
	// Build the runtime-record DBPath the way the daemon does and assert the
	// password is hidden. Direct unit test on the assembly function avoids
	// spinning up the daemon.
	dsn := "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require" //nolint:gosec // fixture
	got := redactRuntimeDSN(dsn)
	assert.NotContains(t, got, "SECRET")
	assert.Contains(t, got, "db.example.com")
	// Mutation guard: the raw DSN really does contain the secret.
	assert.Contains(t, dsn, "SECRET")
}

func TestRuntimeRecordKeepsSQLitePath(t *testing.T) {
	got := redactRuntimeDSN("/var/lib/kata/kata.db")
	assert.Equal(t, "/var/lib/kata/kata.db", got)
}

func TestRuntimeRecordPassesThroughSQLiteSchemeDSN(t *testing.T) {
	// A sqlite:// URL has no credential to redact; the helper must not
	// mangle it. RedactDSN already preserves the userinfo-free form, so
	// the round-trip is identity.
	got := redactRuntimeDSN("sqlite:///var/lib/kata/kata.db")
	assert.Equal(t, "sqlite:///var/lib/kata/kata.db", got)
}

func TestDaemonStart_RejectsAgentOutputBeforeStartup(t *testing.T) {
	for _, args := range [][]string{
		{"--agent", "daemon", "start", "--listen", "8.8.8.8:7777"},
		{"--format", "agent", "daemon", "start", "--listen", "8.8.8.8:7777"},
	} {
		resetFlags(t)
		setupKataEnv(t)

		stdout, stderr, err := executeRootCapture(t, context.Background(), args...)

		require.Error(t, err, "args %v", args)
		ce := requireCLIError(t, err, ExitUsage)
		assert.Equal(t, kindUsage, ce.Kind)
		assert.Contains(t, ce.Message, "kata daemon start does not support --agent")
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "kata daemon start does not support --agent")
		assert.NotContains(t, stderr, "non-public")
	}
}

func TestDaemonStop_AgentReportsStoppedPID(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	child := startSleepProcess(t)
	writeRuntimePID(t, tmp, child.Process.Pid)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "stop")

	assert.Equal(t, "OK daemon action=stop pid="+strconv.Itoa(child.Process.Pid)+"\n", string(out))
}

func TestDaemonStop_AgentNoDaemonReportsNoop(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "stop")

	assert.Equal(t, "OK daemon action=stop stopped=0\n", string(out))
}

func TestDaemonStop_JSONReportsStoppedPIDs(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	child := startSleepProcess(t)
	writeRuntimePID(t, tmp, child.Process.Pid)

	out := executeRoot(t, newRootCmd(), "--json", "daemon", "stop")

	var got struct {
		KataAPIVersion int    `json:"kata_api_version"`
		Action         string `json:"action"`
		Stopped        int    `json:"stopped"`
		PIDs           []int  `json:"pids"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "stop", got.Action)
	assert.Equal(t, 1, got.Stopped)
	assert.Equal(t, []int{child.Process.Pid}, got.PIDs)
}

func TestDaemonStop_JSONReportsNoop(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newRootCmd(), "--json", "daemon", "stop")

	var got struct {
		KataAPIVersion int    `json:"kata_api_version"`
		Action         string `json:"action"`
		Stopped        int    `json:"stopped"`
		PIDs           []int  `json:"pids"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "stop", got.Action)
	assert.Equal(t, 0, got.Stopped)
	assert.Empty(t, got.PIDs)
}

func TestDaemonStop_AgentReportsMultiplePIDs(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	first := startSleepProcess(t)
	second := startSleepProcess(t)
	writeRuntimePID(t, tmp, first.Process.Pid)
	writeRuntimePID(t, tmp, second.Process.Pid)

	out := string(executeRoot(t, newRootCmd(), "--agent", "daemon", "stop"))

	assert.Contains(t, out, "OK daemon action=stop stopped=2 pids=")
	assert.Contains(t, out, strconv.Itoa(first.Process.Pid))
	assert.Contains(t, out, strconv.Itoa(second.Process.Pid))
}

func TestDaemonStop_JSONReportsMultiplePIDs(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	first := startSleepProcess(t)
	second := startSleepProcess(t)
	writeRuntimePID(t, tmp, first.Process.Pid)
	writeRuntimePID(t, tmp, second.Process.Pid)

	out := executeRoot(t, newRootCmd(), "--json", "daemon", "stop")

	var got struct {
		KataAPIVersion int    `json:"kata_api_version"`
		Action         string `json:"action"`
		Stopped        int    `json:"stopped"`
		PIDs           []int  `json:"pids"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "stop", got.Action)
	assert.Equal(t, 2, got.Stopped)
	assert.ElementsMatch(t, []int{first.Process.Pid, second.Process.Pid}, got.PIDs)
}

func TestDaemonReload_AgentReportsReloadedPID(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	child := startSleepProcess(t)
	writeRuntimePID(t, tmp, child.Process.Pid)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "reload")

	assert.Equal(t, "OK daemon action=reload pid="+strconv.Itoa(child.Process.Pid)+"\n", string(out))
}

func TestDaemonReload_JSONReportsReloadedPID(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	child := startSleepProcess(t)
	writeRuntimePID(t, tmp, child.Process.Pid)

	out := executeRoot(t, newRootCmd(), "--json", "daemon", "reload")

	var got struct {
		KataAPIVersion int    `json:"kata_api_version"`
		Action         string `json:"action"`
		PID            int    `json:"pid"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "reload", got.Action)
	assert.Equal(t, child.Process.Pid, got.PID)
}

func TestHealth_AgentReportsOK(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))

	out := executeRoot(t, cmd, "--agent", "health")
	assert.Equal(t, "OK health ok=true daemon=running\n", string(out))
}

func TestDaemonStart_ListenFlagRejectsPublicAddress(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start", "--listen", "8.8.8.8:7777"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-public")
}

func TestDaemonStart_ListenFlagRejectsMalformed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start", "--listen", "not-a-host-port"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--listen")
}

func TestListenFromPortEnv(t *testing.T) {
	t.Run("PORT yields wildcard bind", func(t *testing.T) {
		t.Setenv(daemon.AutoStartMarkerEnv, "")
		t.Setenv("PORT", "8080")
		addr, ok := listenFromPortEnv()
		require.True(t, ok)
		assert.Equal(t, "0.0.0.0:8080", addr)
	})
	t.Run("auto-start marker suppresses PORT reading", func(t *testing.T) {
		// The implicit auto-start child inherits the parent environment,
		// so a stray PORT on a developer's shell must not flip it onto
		// wildcard TCP — the spawner stamps the marker for that reason.
		t.Setenv(daemon.AutoStartMarkerEnv, "1")
		t.Setenv("PORT", "8080")
		_, ok := listenFromPortEnv()
		assert.False(t, ok)
	})
	t.Run("invalid PORT is ignored", func(t *testing.T) {
		t.Setenv(daemon.AutoStartMarkerEnv, "")
		t.Setenv("PORT", "not-a-port")
		_, ok := listenFromPortEnv()
		assert.False(t, ok)
	})
}

// TestDaemonStart_PortEnvBindsWildcard verifies that when the platform
// injects PORT and the daemon is started explicitly (no auto-start
// marker), with no --listen flag and no config value, the bind address
// is derived from PORT as 0.0.0.0:$PORT. With no token configured, the
// auth-startup guard refuses the non-loopback bind — and the refusal
// names the derived address, proving the PORT path was taken and the
// address passed validation.
func TestDaemonStart_PortEnvBindsWildcard(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	t.Setenv(daemon.AutoStartMarkerEnv, "")
	t.Setenv("PORT", "8081")
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0.0.0.0:8081")
}

// TestDaemonStart_ConfigFileListenIsHonored verifies that
// <KATA_HOME>/config.toml's `listen = ...` value is picked up when the
// --listen flag is absent. We use an obviously-public address so the
// validator rejects it before the daemon actually starts — this lets us
// assert that the config value was consulted (otherwise the daemon would
// fall through to the Unix-socket path and not error).
func TestDaemonStart_ConfigFileListenIsHonored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.toml"),
		[]byte(`listen = "8.8.8.8:7777"`+"\n"), 0o600))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-public",
		"config.toml listen value must reach the validator")
}

// TestDaemonStart_FlagWinsOverConfigFile asserts the --listen flag
// takes precedence over <KATA_HOME>/config.toml.
func TestDaemonStart_FlagWinsOverConfigFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	// Config file says one thing, flag says another — flag must win.
	// Both are public so the daemon will reject either, but only the
	// flag's address should appear in the error.
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.toml"),
		[]byte(`listen = "1.1.1.1:7777"`+"\n"), 0o600))

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"daemon", "start", "--listen", "8.8.8.8:7777"})

	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "8.8.8.8")
	assert.NotContains(t, err.Error(), "1.1.1.1",
		"config.toml value must NOT win when --listen is set")
}

func TestNewDaemonTelemetryReporterUsesInstanceUID(t *testing.T) {
	tmp := t.TempDir()
	store := openKataTestDB(t, filepath.Join(tmp, "kata.db"))
	defer func() { _ = store.Close() }()

	var got telemetry.Options
	orig := newTelemetryReporter
	newTelemetryReporter = func(opts telemetry.Options) telemetry.Client {
		got = opts
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = orig })

	reporter := newDaemonTelemetryReporter(store)

	require.NotNil(t, reporter)
	assert.Equal(t, store.InstanceUID(), got.DistinctID)
	assert.NotEmpty(t, got.Version)
	assert.NotEmpty(t, got.Commit)
}

func TestCaptureDaemonStartedTelemetryIncludesProjectCount(t *testing.T) {
	tmp := t.TempDir()
	store := openKataTestDB(t, filepath.Join(tmp, "kata.db"))
	defer func() { _ = store.Close() }()
	_, err := store.CreateProject(t.Context(), "alpha")
	require.NoError(t, err)

	reporter := &fakeTelemetryReporter{}
	captureDaemonStartedTelemetry(t.Context(), store, reporter)

	require.Equal(t, 1, reporter.eventCount())
	event := reporter.eventAt(0)
	assert.Equal(t, "daemon_started", event.event)
	assert.Equal(t, 1, event.properties["project_count"])
}

func TestRunDaemonTelemetryHeartbeatEmitsDailyActiveEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		captures := []time.Time{}

		done := make(chan struct{})
		go func() {
			defer close(done)
			runDaemonTelemetryHeartbeat(ctx, func(context.Context) {
				captures = append(captures, time.Now())
			})
		}()

		synctest.Wait()
		require.Len(t, captures, 1)
		first := captures[0]

		time.Sleep(daemonTelemetryHeartbeatInterval - time.Nanosecond)
		synctest.Wait()
		require.Len(t, captures, 1)

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		require.Len(t, captures, 2)
		assert.Equal(t, first.Add(daemonTelemetryHeartbeatInterval), captures[1])

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("heartbeat goroutine did not exit after cancellation")
		}
	})
}

func TestDefaultEndpointForOS(t *testing.T) {
	ns := &daemon.Namespace{SocketDir: t.TempDir()}

	t.Run("windows uses loopback TCP", func(t *testing.T) {
		ep := defaultEndpointForOS(ns, "windows")
		assert.Equal(t, "tcp", ep.Network)
		assert.Equal(t, "127.0.0.1:0", ep.Address)
	})

	t.Run("unix uses runtime socket", func(t *testing.T) {
		ep := defaultEndpointForOS(ns, "linux")
		assert.Equal(t, "unix", ep.Network)
		assert.Equal(t, "unix://"+filepath.Join(ns.SocketDir, "daemon.sock"), ep.ConfigAddress())
	})
}

type fakeTelemetryReporter struct {
	mu     sync.Mutex
	events []fakeTelemetryEvent
}

type fakeTelemetryEvent struct {
	event      string
	properties map[string]any
}

func (f *fakeTelemetryReporter) Enabled() bool { return true }

func (f *fakeTelemetryReporter) Capture(event string, properties map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fakeTelemetryEvent{event: event, properties: properties})
	return nil
}

func (f *fakeTelemetryReporter) Close() error { return nil }

func (f *fakeTelemetryReporter) eventCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *fakeTelemetryReporter) eventAt(i int) fakeTelemetryEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	event := f.events[i]
	props := make(map[string]any, len(event.properties))
	for key, value := range event.properties {
		props[key] = value
	}
	event.properties = props
	return event
}

func TestRuntimeEndpointForListener_UsesActualTCPPort(t *testing.T) {
	ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:0"}
	l, err := ep.Listen()
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	got := runtimeEndpointForListener(ep, l)

	require.NotEqual(t, ep.Address, got.Address)
	host, port, err := net.SplitHostPort(got.Address)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", port)
}

func TestRuntimeEndpointForListener_KeepsExplicitTCPAddress(t *testing.T) {
	ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:0"}
	l, err := ep.Listen()
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	_, actualPort, err := net.SplitHostPort(runtimeEndpointForListener(ep, l).Address)
	require.NoError(t, err)
	explicit := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:" + actualPort}

	assert.Equal(t, explicit, runtimeEndpointForListener(explicit, l))
}

func TestEnsureDaemon_ReturnsExistingURL(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	tmp := setupKataEnv(t)

	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	require.NoError(t, writeRuntimeFor(tmp, addr))

	url, err := ensureDaemon(context.Background())
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(url, "http://"))
}

func startSleepProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestDaemonCommandSleepHelperProcess", "--") //nolint:gosec // test helper starts this test binary
	cmd.Env = append(os.Environ(), "KATA_DAEMON_CMD_SLEEP_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return cmd
}

func TestDaemonCommandSleepHelperProcess(_ *testing.T) {
	if os.Getenv("KATA_DAEMON_CMD_SLEEP_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func writeRuntimePID(t *testing.T, home string, pid int) {
	t.Helper()
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       pid,
		Network:   "unix",
		Address:   filepath.Join(home, "daemon.sock"),
		Metadata:  map[string]string{"db_path": filepath.Join(home, "kata.db")},
		Version:   "v-test",
		StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	// On Windows, daemon stop/reload signal via per-daemon named events that
	// a real daemon creates at startup (installStopWatcher/installReloadSource).
	// A faked daemon PID has none, so create them here; no-op on Unix, where
	// stop/reload deliver SIGTERM/SIGHUP straight to the PID.
	registerDaemonSignalEndpoints(t, ns.DBHash, pid)
}

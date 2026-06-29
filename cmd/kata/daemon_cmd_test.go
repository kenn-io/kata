package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/hooks"
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

func TestDaemonStart_RuntimeRecordSerializesUnixAddressAsURL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("default daemon endpoint is TCP on Windows")
	}
	ns, got := readRuntimeRecordFromStartedDaemon(t, "")

	assert.Equal(t, "unix", got.Network)
	assert.Equal(t, "unix://"+filepath.Join(ns.SocketDir, "daemon.sock"), got.Address)
}

func TestDaemonStart_RuntimeRecordSerializesTCPAddressAsHostPort(t *testing.T) {
	_, got := readRuntimeRecordFromStartedDaemon(t, "127.0.0.1:0")

	host, port, err := net.SplitHostPort(got.Address)
	require.NoError(t, err)
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", port)
	assert.NotContains(t, got.Address, "://")
}

type daemonRuntimeRecordJSON struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

func readRuntimeRecordFromStartedDaemon(t *testing.T, listen string) (*daemon.Namespace, daemonRuntimeRecordJSON) {
	t.Helper()
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemon.AutoStartMarkerEnv, "1")

	orig := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = orig })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDaemonWithListen(ctx, listen, false)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("daemon did not stop cleanly: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop after context cancellation")
		}
	})

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	runtimePath, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Path(os.Getpid())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := os.Stat(runtimePath)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond)

	body, err := os.ReadFile(runtimePath) //nolint:gosec // G304: runtimePath is generated from test-owned KATA_HOME via RuntimeStore.Path.
	require.NoError(t, err)
	var got daemonRuntimeRecordJSON
	require.NoError(t, json.Unmarshal(body, &got))
	return ns, got
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

func TestDaemonStart_DetachesByDefaultAfterStartup(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	oldStart := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = oldStart })
	var gotListen string
	var gotInsecureReadonly bool
	startDetachedDaemon = func(_ context.Context, listen string, insecureReadonly bool) (daemonStartOutput, error) {
		gotListen = listen
		gotInsecureReadonly = insecureReadonly
		return daemonStartOutput{
			Action:  "started",
			PID:     1234,
			Address: "127.0.0.1:7777",
		}, nil
	}

	stdout, stderr, err := executeRootCapture(t, context.Background(),
		"daemon", "start", "--listen", "127.0.0.1:7777", "--insecure-readonly")

	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7777", gotListen)
	assert.True(t, gotInsecureReadonly)
	assert.Equal(t, "started pid=1234 address=127.0.0.1:7777\n", stdout)
	assert.Empty(t, stderr)
}

func TestDaemonStart_ListenConflictWithExistingDaemon(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, writeRuntimeFor(home, "127.0.0.1:7777"))

	out, err := defaultStartDetachedDaemon(context.Background(), "100.64.0.5:7777", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon already running")
	assert.Contains(t, err.Error(), "127.0.0.1:7777")
	assert.Contains(t, err.Error(), "100.64.0.5:7777")
	assert.Empty(t, out)
}

func TestDaemonStart_ListenMatchesExistingDaemon(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, writeRuntimeFor(home, "100.64.0.5:7777"))

	out, err := defaultStartDetachedDaemon(context.Background(), "100.64.0.5:7777", false)

	require.NoError(t, err)
	assert.Equal(t, "already_running", out.Action)
	assert.Equal(t, os.Getpid(), out.PID)
	assert.Equal(t, "100.64.0.5:7777", out.Address)
}

func TestDaemonStart_ExplicitListenMatchIgnoresMalformedConfig(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, writeRuntimeFor(home, "100.64.0.5:7777"))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("listen =\n"), 0o600))

	out, err := defaultStartDetachedDaemon(context.Background(), "100.64.0.5:7777", false)

	require.NoError(t, err)
	assert.Equal(t, "already_running", out.Action)
	assert.Equal(t, os.Getpid(), out.PID)
	assert.Equal(t, "100.64.0.5:7777", out.Address)
}

func TestDaemonStart_ConfigListenConflictWithExistingDaemon(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	require.NoError(t, writeRuntimeFor(home, "127.0.0.1:7777"))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte(`listen = "100.64.0.5:7777"`+"\n"), 0o600))

	out, err := defaultStartDetachedDaemon(context.Background(), "", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon already running")
	assert.Contains(t, err.Error(), "127.0.0.1:7777")
	assert.Contains(t, err.Error(), "100.64.0.5:7777")
	assert.Empty(t, out)
}

func TestDaemonStart_PortListenConflictWithExistingDaemon(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("PORT", "8080")
	require.NoError(t, writeRuntimeFor(home, "127.0.0.1:7777"))

	out, err := defaultStartDetachedDaemon(context.Background(), "", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon already running")
	assert.Contains(t, err.Error(), "127.0.0.1:7777")
	assert.Contains(t, err.Error(), "0.0.0.0:8080")
	assert.Empty(t, out)
}

func TestDaemonStart_ForegroundKeepsCurrentProcess(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	oldStart := startDetachedDaemon
	oldRun := runDaemonForeground
	t.Cleanup(func() {
		startDetachedDaemon = oldStart
		runDaemonForeground = oldRun
	})
	var detachedCalled bool
	startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
		detachedCalled = true
		return daemonStartOutput{}, nil
	}
	var gotListen string
	var gotInsecureReadonly bool
	runDaemonForeground = func(_ context.Context, listen string, insecureReadonly bool) error {
		gotListen = listen
		gotInsecureReadonly = insecureReadonly
		return nil
	}

	stdout, stderr, err := executeRootCapture(t, context.Background(),
		"daemon", "start", "--foreground", "--listen", "127.0.0.1:7777", "--insecure-readonly")

	require.NoError(t, err)
	assert.False(t, detachedCalled)
	assert.Equal(t, "127.0.0.1:7777", gotListen)
	assert.True(t, gotInsecureReadonly)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
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
	cmd.SetArgs([]string{"daemon", "start", "--foreground", "--listen", "8.8.8.8:7777"})

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
	cmd.SetArgs([]string{"daemon", "start", "--foreground", "--listen", "not-a-host-port"})

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
// injects PORT and the daemon is started explicitly in the foreground (no auto-start
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
	cmd.SetArgs([]string{"daemon", "start", "--foreground"})

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
	cmd.SetArgs([]string{"daemon", "start", "--foreground"})

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
	cmd.SetArgs([]string{"daemon", "start", "--foreground", "--listen", "8.8.8.8:7777"})

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
		var capturesMu sync.Mutex
		captures := []time.Time{}
		capturesSnapshot := func() []time.Time {
			capturesMu.Lock()
			defer capturesMu.Unlock()
			return append([]time.Time(nil), captures...)
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			runDaemonTelemetryHeartbeat(ctx, func(context.Context) {
				capturesMu.Lock()
				captures = append(captures, time.Now())
				capturesMu.Unlock()
			})
		}()

		synctest.Wait()
		got := capturesSnapshot()
		require.Len(t, got, 1)
		first := got[0]

		time.Sleep(daemonTelemetryHeartbeatInterval - time.Nanosecond)
		synctest.Wait()
		require.Len(t, capturesSnapshot(), 1)

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		got = capturesSnapshot()
		require.Len(t, got, 2)
		assert.Equal(t, first.Add(daemonTelemetryHeartbeatInterval), got[1])

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("heartbeat goroutine did not exit after cancellation")
		}
	})
}

func TestGitHubSyncRunnerInterval(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "")
	assert.Equal(t, 5*time.Minute, githubSyncRunnerInterval())

	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "25")
	assert.Equal(t, 25*time.Millisecond, githubSyncRunnerInterval())

	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "0")
	assert.Equal(t, 5*time.Minute, githubSyncRunnerInterval())

	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "not-a-number")
	assert.Equal(t, 5*time.Minute, githubSyncRunnerInterval())
}

func TestDaemonGitHubSyncHTTPFetcherUsesCredentialConfig(t *testing.T) {
	var captured githubsync.HTTPFetcherConfig
	orig := newGitHubSyncHTTPFetcher
	newGitHubSyncHTTPFetcher = func(cfg githubsync.HTTPFetcherConfig) *githubsync.HTTPFetcher {
		captured = cfg
		return orig(cfg)
	}
	t.Cleanup(func() { newGitHubSyncHTTPFetcher = orig })

	tokenEnv := "EXAMPLE_" + "GITHUB_TOKEN"
	cfg := config.GitHubSyncConfig{
		TokenEnv: tokenEnv,
		Apps: []config.GitHubAppConfig{{
			Host:           "github.com",
			Owner:          "example-owner",
			AppID:          123,
			InstallationID: 456,
			PrivateKeyPath: "/secure/example.pem",
		}},
	}
	fetcher := newConfiguredGitHubSyncFetcher(cfg)
	require.IsType(t, &githubsync.HTTPFetcher{}, fetcher)

	resolver, ok := captured.CredentialResolver.(*githubsync.CredentialResolver)
	require.True(t, ok)
	appKind, err := resolver.ResolveKind(context.Background(), githubsync.Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)
	assert.Equal(t, githubsync.CredentialKindApp, appKind)

	t.Setenv(tokenEnv, "env-token")
	envKind, err := resolver.ResolveKind(context.Background(), githubsync.Binding{
		Host:  "github.com",
		Owner: "other-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)
	assert.Equal(t, githubsync.CredentialKindEnv, envKind)
}

func TestDaemonStartGitHubSyncHTTPFetcherUsesConfigFileCredentials(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemon.AutoStartMarkerEnv, "1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[github_sync]
token_env = "EXAMPLE_GITHUB_TOKEN"

[[github_sync.app]]
host = "github.com"
owner = "example-owner"
app_id = 123
installation_id = 456
private_key_path = "/secure/example.pem"
`), 0o600))

	origTelemetry := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = origTelemetry })

	captured := make(chan githubsync.HTTPFetcherConfig, 1)
	origFetcher := newGitHubSyncHTTPFetcher
	newGitHubSyncHTTPFetcher = func(cfg githubsync.HTTPFetcherConfig) *githubsync.HTTPFetcher {
		select {
		case captured <- cfg:
		default:
		}
		return origFetcher(cfg)
	}
	t.Cleanup(func() { newGitHubSyncHTTPFetcher = origFetcher })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDaemonWithListen(ctx, "127.0.0.1:0", false)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("daemon did not stop cleanly: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop after context cancellation")
		}
	})

	var cfg githubsync.HTTPFetcherConfig
	select {
	case cfg = <-captured:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not construct GitHub sync HTTP fetcher")
	}
	resolver, ok := cfg.CredentialResolver.(*githubsync.CredentialResolver)
	require.True(t, ok)
	kind, err := resolver.ResolveKind(context.Background(), githubsync.Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)
	assert.Equal(t, githubsync.CredentialKindApp, kind)

	t.Setenv("EXAMPLE_GITHUB_TOKEN", "env-token")
	envKind, err := resolver.ResolveKind(context.Background(), githubsync.Binding{
		Host:  "github.com",
		Owner: "other-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)
	assert.Equal(t, githubsync.CredentialKindEnv, envKind)
}

func TestDaemonStartGitHubSyncRunnerCreatesOneRunnerWithDaemonDBAndFetcher(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "25")
	store := openKataTestDB(t, filepath.Join(t.TempDir(), "kata.db"))
	defer func() { _ = store.Close() }()
	fetcher := &daemonGitHubSyncFetcher{}
	bcast := daemon.NewEventBroadcaster()
	runner := &recordingGitHubSyncDaemonRunner{runCalled: make(chan struct{})}
	var configs []githubsync.RunnerConfig
	orig := newGitHubSyncDaemonRunner
	newGitHubSyncDaemonRunner = func(cfg githubsync.RunnerConfig) githubSyncDaemonRunner {
		configs = append(configs, cfg)
		return runner
	}
	t.Cleanup(func() { newGitHubSyncDaemonRunner = orig })

	ctx, cancel := context.WithCancel(context.Background())
	wake := startGitHubSyncRunner(ctx, store, fetcher, bcast, hooks.NewNoop(), log.New(io.Discard, "", 0))
	defer cancel()

	require.Eventually(t, func() bool {
		return runner.wasRun()
	}, time.Second, time.Millisecond)
	require.Len(t, configs, 1)
	assert.Same(t, store, configs[0].Store)
	assert.Same(t, fetcher, configs[0].Fetcher)
	assert.Equal(t, 25*time.Millisecond, configs[0].Interval)
	assert.NotNil(t, configs[0].Wake)
	assert.NotNil(t, configs[0].EventSink)
	assert.NotNil(t, configs[0].Logger)
	require.NotNil(t, wake)
	require.NotPanics(t, wake)
}

func TestDaemonStartGitHubSyncRunnerNilFetcherUsesHTTPFetcher(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "25")
	store := openKataTestDB(t, filepath.Join(t.TempDir(), "kata.db"))
	defer func() { _ = store.Close() }()
	runner := &recordingGitHubSyncDaemonRunner{runCalled: make(chan struct{})}
	var configs []githubsync.RunnerConfig
	orig := newGitHubSyncDaemonRunner
	newGitHubSyncDaemonRunner = func(cfg githubsync.RunnerConfig) githubSyncDaemonRunner {
		configs = append(configs, cfg)
		return runner
	}
	t.Cleanup(func() { newGitHubSyncDaemonRunner = orig })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startGitHubSyncRunner(ctx, store, nil, daemon.NewEventBroadcaster(), hooks.NewNoop(), log.New(io.Discard, "", 0))

	require.Eventually(t, func() bool {
		return runner.wasRun()
	}, time.Second, time.Millisecond)
	require.Len(t, configs, 1)
	require.IsType(t, &githubsync.HTTPFetcher{}, configs[0].Fetcher)
}

func TestDaemonGitHubSyncRunnerTickerSyncsDueBindingWithoutManualOnce(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "10")
	store, project, binding := newDaemonGitHubSyncStore(t)
	fetcher := newDaemonGitHubSyncFetcher(binding)
	fetcher.issues = []githubsync.Issue{daemonGitHubSyncIssue(101, 1, "first issue")}
	bcast := daemon.NewEventBroadcaster()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startGitHubSyncRunner(ctx, store, fetcher, bcast, hooks.NewNoop(), log.New(io.Discard, "", 0))

	require.Eventually(t, func() bool {
		got, err := store.IssueSyncBindingByID(context.Background(), binding.ID)
		return err == nil && got.LastCursorAt != nil
	}, time.Second, time.Millisecond)
	status, err := store.IssueSyncStatusByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, status.LastCreated)
	assert.Equal(t, int64(1), fetcher.repositoryCallCount())
}

func TestDaemonGitHubSyncRunnerBroadcastsNativeImportEvents(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "10")
	store, project, binding := newDaemonGitHubSyncStore(t)
	fetcher := newDaemonGitHubSyncFetcher(binding)
	fetcher.issues = []githubsync.Issue{daemonGitHubSyncIssue(101, 1, "first issue")}
	bcast := daemon.NewEventBroadcaster()
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	hookSink := &recordingDaemonHookSink{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startGitHubSyncRunner(ctx, store, fetcher, bcast, hookSink, log.New(io.Discard, "", 0))

	var msg daemon.StreamMsg
	select {
	case msg = <-sub.Ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for GitHub sync import event")
	}
	require.Equal(t, "event", msg.Kind)
	require.NotNil(t, msg.Event)
	assert.Equal(t, project.ID, msg.ProjectID)
	assert.Equal(t, []int64{msg.Event.ID}, hookSink.eventIDs())

	select {
	case extra := <-sub.Ch:
		t.Fatalf("unexpected duplicate GitHub sync event: %#v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDaemonGitHubSyncRunnerDoesNotOverlapWakeWhileBindingIsInFlight(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_INTERVAL_MS", "10")
	store, _, binding := newDaemonGitHubSyncStore(t)
	fetcher := newDaemonGitHubSyncFetcher(binding)
	fetcher.issues = []githubsync.Issue{daemonGitHubSyncIssue(101, 1, "first issue")}
	fetcher.blockRepository = make(chan struct{})
	fetcher.releaseRepository = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := startGitHubSyncRunner(ctx, store, fetcher, daemon.NewEventBroadcaster(), hooks.NewNoop(), log.New(io.Discard, "", 0))

	select {
	case <-fetcher.blockRepository:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for repository fetch")
	}
	for i := 0; i < 5; i++ {
		wake()
	}
	require.Never(t, func() bool {
		return fetcher.repositoryCallCount() > 1
	}, 50*time.Millisecond, time.Millisecond)

	close(fetcher.releaseRepository)
	require.Eventually(t, func() bool {
		got, err := store.IssueSyncBindingByID(context.Background(), binding.ID)
		return err == nil && got.LastCursorAt != nil
	}, time.Second, time.Millisecond)
	assert.Equal(t, int64(1), fetcher.repositoryCallCount())
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

type recordingGitHubSyncDaemonRunner struct {
	mu        sync.Mutex
	runCalled chan struct{}
	closed    bool
}

func (r *recordingGitHubSyncDaemonRunner) Run(ctx context.Context) error {
	r.mu.Lock()
	if !r.closed {
		close(r.runCalled)
		r.closed = true
	}
	r.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (r *recordingGitHubSyncDaemonRunner) wasRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func newDaemonGitHubSyncStore(t *testing.T) (*sqlitestore.Store, db.Project, db.IssueSyncBinding) {
	t.Helper()
	store := openKataTestDB(t, filepath.Join(t.TempDir(), "kata.db"))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID:       project.ID,
		Provider:        "github",
		SourceKey:       "github:R_example_repo",
		RemoteID:        "R_example_repo",
		DisplayName:     "example-owner/example-repo",
		Config:          mustCmdGitHubSyncConfig(t, "github.com", "example-owner", "example-repo", 101),
		IntervalSeconds: 300,
	})
	require.NoError(t, err)
	return store, project, binding
}

type daemonGitHubSyncFetcher struct {
	mu                sync.Mutex
	repo              githubsync.Repository
	issues            []githubsync.Issue
	comments          map[int][]githubsync.Comment
	repositoryCalls   int64
	blockRepository   chan struct{}
	releaseRepository chan struct{}
	blockOnce         sync.Once
}

func newDaemonGitHubSyncFetcher(binding db.IssueSyncBinding) *daemonGitHubSyncFetcher {
	return &daemonGitHubSyncFetcher{
		repo: githubsync.Repository{
			NodeID:   binding.RemoteID,
			ID:       101,
			FullName: binding.DisplayName,
		},
		comments: map[int][]githubsync.Comment{},
	}
}

func (f *daemonGitHubSyncFetcher) Repository(ctx context.Context, _, _, _ string) (githubsync.Repository, error) {
	f.mu.Lock()
	f.repositoryCalls++
	f.mu.Unlock()
	if f.blockRepository != nil {
		f.blockOnce.Do(func() {
			f.blockRepository <- struct{}{}
			select {
			case <-ctx.Done():
			case <-f.releaseRepository:
			}
		})
	}
	return f.repo, nil
}

func (f *daemonGitHubSyncFetcher) Issues(_ context.Context, _ githubsync.Binding, _ *time.Time) ([]githubsync.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]githubsync.Issue(nil), f.issues...), nil
}

func (f *daemonGitHubSyncFetcher) Comments(_ context.Context, _ githubsync.Binding, issueNumber int) ([]githubsync.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]githubsync.Comment(nil), f.comments[issueNumber]...), nil
}

func (f *daemonGitHubSyncFetcher) ParentData(_ context.Context, _ githubsync.Binding) (githubsync.ParentData, error) {
	return githubsync.ParentData{}, nil
}

func (f *daemonGitHubSyncFetcher) repositoryCallCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repositoryCalls
}

func daemonGitHubSyncIssue(id int64, number int, title string) githubsync.Issue {
	ts := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	return githubsync.Issue{
		ID:        id,
		NodeID:    "I_example",
		Number:    number,
		HTMLURL:   "https://github.com/example-owner/example-repo/issues/1",
		Title:     title,
		Body:      "body",
		State:     "open",
		User:      &githubsync.User{Login: "author"},
		CreatedAt: &ts,
		UpdatedAt: &ts,
	}
}

type recordingDaemonHookSink struct {
	mu     sync.Mutex
	events []db.Event
}

func (s *recordingDaemonHookSink) Enqueue(evt db.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
}

func (s *recordingDaemonHookSink) eventIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.events))
	for _, evt := range s.events {
		out = append(out, evt.ID)
	}
	return out
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

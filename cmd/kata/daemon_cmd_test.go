package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonStatus_NoDaemonReportsAbsent(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	out := executeRoot(t, newDaemonCmd(), "status")
	assert.Equal(t, "No kata daemon is running.\n", string(out))
}

func TestDaemonLocate_JSONReportsConfiguredRemoteWithoutSecrets(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	t.Setenv("KATA_SERVER", "http://"+addr)
	t.Setenv("KATA_AUTH_TOKEN", "secret-that-must-not-be-emitted")

	out := executeRoot(t, newRootCmd(), "daemon", "locate", "--json")

	var got struct {
		KataAPIVersion int    `json:"kata_api_version"`
		Source         string `json:"source"`
		Kind           string `json:"kind"`
		Network        string `json:"network"`
		Scheme         string `json:"scheme"`
		Address        string `json:"address"`
		RequestBaseURL string `json:"request_base_url"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "configured", got.Source)
	assert.Equal(t, "remote", got.Kind)
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "http", got.Scheme)
	assert.Equal(t, "http://"+addr, got.Address)
	assert.Equal(t, "http://"+addr, got.RequestBaseURL)
	assert.NotContains(t, string(out), "secret-that-must-not-be-emitted")
}

func TestDaemonLocate_JSONReportsNamedRemoteWithoutCatalogToken(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	t.Setenv("KATA_EXAMPLE_REMOTE_TOKEN", "catalog-secret-that-must-not-be-emitted")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(`
[[daemon]]
name = "example-remote"
url = "http://%s"
allow_insecure = true
token_env = "KATA_EXAMPLE_REMOTE_TOKEN"
`, addr)), 0o600))

	out := executeRoot(t, newRootCmd(),
		"--daemon", "example-remote", "daemon", "locate", "--json")

	var got daemonLocateOutput
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "daemon_flag", got.Source)
	assert.Equal(t, "remote", got.Kind)
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "http", got.Scheme)
	assert.Equal(t, "http://"+addr, got.Address)
	assert.NotContains(t, string(out), "catalog-secret-that-must-not-be-emitted")
}

func TestDaemonLocate_JSONReportsActiveRemoteWithoutResolvingCatalogToken(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	t.Setenv("KATA_MISSING_REMOTE_TOKEN", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(`
active_daemon = "example-remote"

[[daemon]]
name = "example-remote"
url = "http://%s"
allow_insecure = true
token_env = "KATA_MISSING_REMOTE_TOKEN"
`, addr)), 0o600))

	out := executeRoot(t, newRootCmd(), "daemon", "locate", "--json")

	var got daemonLocateOutput
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "configured", got.Source)
	assert.Equal(t, "remote", got.Kind)
	assert.Equal(t, "http://"+addr, got.Address)
}

func TestDaemonLocate_ErrorRedactsConfiguredURLUserInfo(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "http://fixture-user:fixture-secret@127.0.0.1:7777")

	_, stderr, err := executeRootCapture(t, t.Context(), "daemon", "locate")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fixture-user")
	assert.NotContains(t, err.Error(), "fixture-secret")
	assert.NotContains(t, stderr, "fixture-user")
	assert.NotContains(t, stderr, "fixture-secret")
}

func TestDaemonLocate_ErrorRedactsMalformedConfiguredURLUserInfo(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "http://fixture-user:fixture-secret@127.0.0.1:bad")

	_, stderr, err := executeRootCapture(t, t.Context(), "daemon", "locate")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fixture-user")
	assert.NotContains(t, err.Error(), "fixture-secret")
	assert.NotContains(t, stderr, "fixture-user")
	assert.NotContains(t, stderr, "fixture-secret")
}

func TestDaemonLocate_ErrorRedactsConfiguredURLPathQueryAndFragment(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "http://public.example/secret-path?token=secret-query#secret-fragment")

	_, stderr, err := executeRootCapture(t, t.Context(), "daemon", "locate")

	require.Error(t, err)
	for _, secret := range []string{"secret-path", "secret-query", "secret-fragment"} {
		assert.NotContains(t, err.Error(), secret)
		assert.NotContains(t, stderr, secret)
	}
}

func TestDaemonLocate_JSONReportsLocalTCPConfigAddress(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	require.NoError(t, writeRuntimeFor(home, addr))

	out := executeRoot(t, newRootCmd(), "daemon", "locate", "--json")

	var got daemonLocateOutput
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "local_default", got.Source)
	assert.Equal(t, "local", got.Kind)
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "http", got.Scheme)
	assert.Equal(t, addr, got.Address)
	assert.Equal(t, "http://"+addr, got.RequestBaseURL)
}

func TestDaemonLocate_JSONReportsLocalUnixConfigAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unavailable on Windows")
	}
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "1")
	socketDir := filepath.Join(t.TempDir(), "socket dir")
	require.NoError(t, os.Mkdir(socketDir, 0o700))
	socketPath := filepath.Join(socketDir, "kata.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	go func() { _ = http.Serve(listener, mux) }() //nolint:gosec // test-only Unix socket

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: os.Getpid(), Network: "unix", Address: socketPath,
	})
	require.NoError(t, err)

	out := executeRoot(t, newRootCmd(), "daemon", "locate", "--json")

	var got daemonLocateOutput
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "local_default", got.Source)
	assert.Equal(t, "local", got.Kind)
	assert.Equal(t, "unix", got.Network)
	assert.Equal(t, "http", got.Scheme)
	assert.Equal(t, "unix://"+socketPath, got.Address)
	assert.Empty(t, got.RequestBaseURL)

	agent := executeRoot(t, newRootCmd(), "--agent", "daemon", "locate")
	assert.Equal(t,
		"OK daemon source=local_default kind=local network=unix scheme=http address="+
			agentValue("unix://"+socketPath)+"\n",
		string(agent))
}

func TestDaemonLocate_HumanAndAgentOutput(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	t.Setenv("KATA_SERVER", "http://"+addr)

	human := executeRoot(t, newRootCmd(), "daemon", "locate")
	assert.Equal(t, "http://"+addr+"\n", string(human))

	agent := executeRoot(t, newRootCmd(), "--agent", "daemon", "locate")
	assert.Equal(t,
		"OK daemon source=configured kind=remote network=tcp scheme=http address=http://"+addr+
			" request_base_url=http://"+addr+"\n",
		string(agent))
}

func TestDaemonLocate_HelpDocumentsResolutionOrder(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"daemon", "locate"})
	require.NoError(t, err)

	wantInOrder := []string{
		"--daemon", "KATA_SERVER", ".kata.local.toml", "active_daemon", "local daemon",
	}
	last := -1
	for _, want := range wantInOrder {
		index := strings.Index(cmd.Long, want)
		assert.Greater(t, index, last, "%q should follow the prior resolution source", want)
		last = index
	}
	assert.Contains(t, cmd.Long, "unix:///path")
	assert.Contains(t, cmd.Long, "host:port")
	assert.Contains(t, cmd.Long, "never includes authentication credentials")
}

func TestDaemonStatus_HumanReportsLifecycleDetails(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	started := time.Now().Add(-52*time.Minute - 33*time.Second)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   "tcp",
		Address:   "127.0.0.1:7777",
		Version:   "v-test-status",
		StartedAt: started,
		Metadata:  map[string]string{"web_origin": "http://127.0.0.1:28888"},
	})
	require.NoError(t, err)

	out := string(executeRoot(t, newRootCmd(), "daemon", "status"))

	assert.Contains(t, out, "kata running at http://127.0.0.1:7777\n")
	assert.Contains(t, out, "  web UI:  http://127.0.0.1:28888\n")
	assert.Contains(t, out, "  pid:     "+strconv.Itoa(os.Getpid())+"\n")
	assert.Contains(t, out, "  version: v-test-status\n")
	assert.Regexp(t, `(?m)^  uptime:  52m3[3-4]s$`, out)
}

func TestDaemonStatus_JSONReportsDaemonsWithVersion(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	started := time.Date(2026, 5, 4, 1, 2, 3, 0, time.UTC)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: "unix",
		Address: "/tmp/kata-test.sock",
		Metadata: map[string]string{
			"db_path":    filepath.Join(tmp, "kata.db"),
			"web_origin": "http://127.0.0.1:28888",
		},
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
			WebURL    string `json:"web_url"`
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
	assert.Equal(t, "http://127.0.0.1:28888", got.Daemons[0].WebURL)
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

func TestDaemonStatus_IgnoresRuntimeRecordWhosePIDWasReused(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	child := startSleepProcess(t)
	writeReusedRuntimePID(t, tmp, child.Process.Pid)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "status")

	assert.Equal(t, "OK daemon status=stopped\n", string(out))
}

func TestDaemonStatus_AgentReportsWebURL(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:7777",
		Metadata: map[string]string{"web_origin": "http://127.0.0.1:28888"},
	})
	require.NoError(t, err)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "status")
	assert.Equal(t, "OK daemon status=running web_url=http://127.0.0.1:28888\n", string(out))
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

func TestDaemonServesHealthFromPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemon.AutoStartMarkerEnv, "1")
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_DSN", dsn)

	orig := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = orig })

	daemonCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- runDaemonWithListen(daemonCtx, "127.0.0.1:0", false)
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
	var runtimeRecord daemonRuntimeRecordJSON
	require.Eventually(t, func() bool {
		body, readErr := os.ReadFile(runtimePath) //nolint:gosec // test-owned runtime path
		return readErr == nil && json.Unmarshal(body, &runtimeRecord) == nil
	}, 5*time.Second, 20*time.Millisecond)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + runtimeRecord.Address + "/api/v1/health") //nolint:noctx // bounded test client
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var health struct {
		OK            bool   `json:"ok"`
		DBPath        string `json:"db_path"`
		SchemaVersion int    `json:"schema_version"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&health))
	wantIdentity, err := config.CanonicalDSNIdentity(dsn)
	require.NoError(t, err)
	assert.True(t, health.OK)
	assert.Equal(t, db.CurrentSchemaVersion(), health.SchemaVersion)
	assert.Equal(t, wantIdentity, health.DBPath)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+runtimeRecord.Address+"/api/v1/projects",
		bytes.NewBufferString(`{"name":"postgres-daemon","actor":"user-a"}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	createdResponse, err := client.Do(request)
	require.NoError(t, err)
	defer func() { _ = createdResponse.Body.Close() }()
	assert.Equal(t, http.StatusOK, createdResponse.StatusCode)
	var created struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
		Created bool `json:"created"`
	}
	require.NoError(t, json.NewDecoder(createdResponse.Body).Decode(&created))
	assert.True(t, created.Created)
	assert.Equal(t, "postgres-daemon", created.Project.Name)
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

func TestAutostartDaemonPublishesRuntimeThenExitsAfterIdleTimeout(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemon.AutoStartMarkerEnv, "1")
	t.Setenv("KATA_AUTOSTART_IDLE_TIMEOUT", "10s")

	orig := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = orig })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runDaemonWithListen(ctx, "", false)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop during test cleanup")
		}
	})

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	runtimePath, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Path(os.Getpid())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(runtimePath)
		return statErr == nil
	}, 3*time.Second, 10*time.Millisecond, "daemon did not publish its runtime record")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("auto-started daemon did not exit after its idle timeout")
	}
	_, err = os.Stat(runtimePath)
	require.ErrorIs(t, err, os.ErrNotExist, "idle exit left a discoverable runtime record")
}

func TestDaemonDoesNotPublishRuntimeBeforeEmbeddingInitializationCompletes(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("PORT", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[search.embeddings]
base_url = "http://127.0.0.1:1"
model = "example-model"
`), 0o600))

	originalTelemetry := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = originalTelemetry })

	originalOpen := openEmbeddingVectorIndex
	initializing := make(chan struct{})
	releaseInitialization := make(chan struct{})
	var releaseOnce sync.Once
	openEmbeddingVectorIndex = func(context.Context, db.Storage, string) (*vector.Index, error) {
		close(initializing)
		<-releaseInitialization
		return nil, errors.New("test embedding index failure")
	}
	t.Cleanup(func() { openEmbeddingVectorIndex = originalOpen })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runDaemonWithListen(ctx, "127.0.0.1:0", false)
	}()
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(releaseInitialization) })
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop during test cleanup")
		}
	})

	select {
	case <-initializing:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not begin embedding initialization")
	}
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	runtimePath, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Path(os.Getpid())
	require.NoError(t, err)
	_, err = os.Stat(runtimePath)
	require.ErrorIs(t, err, os.ErrNotExist,
		"daemon advertised readiness while embedding initialization was blocked")

	releaseOnce.Do(func() { close(releaseInitialization) })
	select {
	case runErr := <-done:
		require.ErrorContains(t, runErr, "test embedding index failure")
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not return after embedding initialization failed")
	}
	_, err = os.Stat(runtimePath)
	require.ErrorIs(t, err, os.ErrNotExist)
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
			WebURL:  "http://127.0.0.1:28888",
		}, nil
	}

	stdout, stderr, err := executeRootCapture(t, context.Background(),
		"daemon", "start", "--listen", "127.0.0.1:7777", "--insecure-readonly")

	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7777", gotListen)
	assert.True(t, gotInsecureReadonly)
	assert.Equal(t, "started pid=1234 address=127.0.0.1:7777\n"+
		"  web UI:  http://127.0.0.1:28888\n", stdout)
	assert.Empty(t, stderr)
}

func TestDaemonStartOutputFromRecordIncludesWebURL(t *testing.T) {
	record := kitdaemon.RuntimeRecord{
		PID: 1234, Network: "unix", Address: "/tmp/example.sock",
		Metadata: map[string]string{
			"db_path":    "/tmp/example.db",
			"web_origin": "http://127.0.0.1:28888",
		},
	}

	out := daemonStartOutputFromRecord("started", record)

	assert.Equal(t, "started", out.Action)
	assert.Equal(t, 1234, out.PID)
	assert.Equal(t, "unix:///tmp/example.sock", out.Address)
	assert.Equal(t, "/tmp/example.db", out.DBPath)
	assert.Equal(t, "http://127.0.0.1:28888", out.WebURL)
}

func writeIdleDaemonHealthServer(t *testing.T, idleShutdown bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/health":
			if idleShutdown {
				_, _ = fmt.Fprintf(writer,
					`{"ok":true,"api_schema_version":%q,"idle_shutdown":{"timeout":"15m0s","state":"armed"}}`,
					daemon.APISchemaVersion)
				return
			}
			_, _ = fmt.Fprintf(writer, `{"ok":true,"api_schema_version":%q}`, daemon.APISchemaVersion)
		case "/api/v1/ping":
			_, _ = writer.Write([]byte(`{"ok":true,"service":"kata"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writeRuntimeRecordFor(t *testing.T, home, addr string, pid int) {
	t.Helper()
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       pid,
		Address:   addr,
		Metadata:  map[string]string{"db_path": filepath.Join(home, "kata.db")},
		StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
}

func TestDaemonStart_ReplacesIdleAutostartDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test helper does not install the Windows daemon stop event watcher")
	}
	resetFlags(t)
	home := setupKataEnv(t)
	child := startSleepProcess(t)
	exited := make(chan struct{})
	go func() {
		_ = child.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-exited
	})
	server := writeIdleDaemonHealthServer(t, true)
	writeRuntimeRecordFor(t, home, strings.TrimPrefix(server.URL, "http://"), child.Process.Pid)

	orig := launchDetachedDaemon
	t.Cleanup(func() { launchDetachedDaemon = orig })
	launchDetachedDaemon = func(context.Context, string, string, bool) (daemonStartOutput, error) {
		select {
		case <-exited:
			return daemonStartOutput{Action: "started", PID: 4243, Address: "127.0.0.1:7777"}, nil
		default:
			return daemonStartOutput{}, errors.New("replacement started before the idle daemon stopped")
		}
	}

	out, err := defaultStartDetachedDaemon(context.Background(), "", false)

	require.NoError(t, err)
	assert.Equal(t, "replaced", out.Action)
	assert.Equal(t, 4243, out.PID)
	assert.Equal(t, child.Process.Pid, out.ReplacedPID)
}

func TestDaemonStart_KeepsResidentDaemonWithoutIdleShutdown(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	server := writeIdleDaemonHealthServer(t, false)
	address := strings.TrimPrefix(server.URL, "http://")
	writeRuntimeRecordFor(t, home, address, os.Getpid())

	orig := launchDetachedDaemon
	t.Cleanup(func() { launchDetachedDaemon = orig })
	launchDetachedDaemon = func(context.Context, string, string, bool) (daemonStartOutput, error) {
		return daemonStartOutput{}, errors.New("resident daemon must not be replaced")
	}

	out, err := defaultStartDetachedDaemon(context.Background(), "", false)

	require.NoError(t, err)
	assert.Equal(t, "already_running", out.Action)
	assert.Equal(t, os.Getpid(), out.PID)
	assert.Equal(t, address, out.Address)
}

func TestDaemonStart_ReportsReplacedDaemon(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	oldStart := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = oldStart })
	startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
		return daemonStartOutput{
			Action: "replaced", PID: 1234, ReplacedPID: 1111, Address: "127.0.0.1:7777",
		}, nil
	}

	stdout, stderr, err := executeRootCapture(t, context.Background(), "daemon", "start")

	require.NoError(t, err)
	assert.Equal(t, "replaced auto-started daemon pid=1111; started pid=1234 address=127.0.0.1:7777\n", stdout)
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

func TestDaemonStop_DoesNotSignalRuntimeRecordWhosePIDWasReused(t *testing.T) {
	resetFlags(t)
	tmp := setupKataEnv(t)
	child := startSleepProcess(t)
	writeReusedRuntimePID(t, tmp, child.Process.Pid)

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "stop")

	assert.Equal(t, "OK daemon action=stop stopped=0\n", string(out))
	assert.True(t, kitdaemon.ProcessAlive(child.Process.Pid))
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

func TestDaemonRestart_StartsWhenNoDaemonIsRunning(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	orig := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = orig })
	startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
		return daemonStartOutput{
			Action:  "started",
			PID:     4242,
			Address: "127.0.0.1:7777",
			WebURL:  "http://127.0.0.1:28888",
		}, nil
	}

	out := executeRoot(t, newRootCmd(), "daemon", "restart")

	assert.Equal(t, "started pid=4242 address=127.0.0.1:7777 (was not running)\n"+
		"  web UI:  http://127.0.0.1:28888\n", string(out))
}

func TestDaemonRestart_StopsRunningDaemonBeforeStarting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test helper does not install the Windows daemon stop event watcher")
	}
	resetFlags(t)
	tmp := setupKataEnv(t)

	child := exec.Command(os.Args[0], "-test.run=TestDaemonCommandSleepHelperProcess", "--") //nolint:gosec // test helper starts this test binary
	child.Env = append(os.Environ(), "KATA_DAEMON_CMD_SLEEP_HELPER=1")
	require.NoError(t, child.Start())
	exited := make(chan struct{})
	go func() {
		_ = child.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-exited
	})
	writeRuntimePID(t, tmp, child.Process.Pid)

	orig := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = orig })
	startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
		select {
		case <-exited:
			return daemonStartOutput{
				Action: "started", PID: 4243, Address: "127.0.0.1:7777",
				WebURL: "http://127.0.0.1:28888",
			}, nil
		default:
			return daemonStartOutput{}, errors.New("new daemon started before old daemon stopped")
		}
	}

	out := executeRoot(t, newRootCmd(), "daemon", "restart")

	assert.Equal(t, "restarted pid=4243 address=127.0.0.1:7777\n"+
		"  web UI:  http://127.0.0.1:28888\n", string(out))
}

func TestDaemonRestart_AllowsFullGracefulShutdownBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test helper does not install the Windows daemon stop event watcher")
	}
	resetFlags(t)
	tmp := setupKataEnv(t)
	readyPath := filepath.Join(tmp, "shutdown-delay-ready")

	child := exec.Command(os.Args[0], "-test.run=TestDaemonCommandSleepHelperProcess", "--") //nolint:gosec // test helper starts this test binary
	child.Env = append(os.Environ(),
		"KATA_DAEMON_CMD_SLEEP_HELPER=1",
		"KATA_DAEMON_CMD_SHUTDOWN_DELAY=4s",
		"KATA_DAEMON_CMD_READY_PATH="+readyPath,
	)
	require.NoError(t, child.Start())
	exited := make(chan struct{})
	go func() {
		_ = child.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-exited
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, time.Second, 10*time.Millisecond)
	writeRuntimePID(t, tmp, child.Process.Pid)

	orig := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = orig })
	startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
		return daemonStartOutput{Action: "started", PID: 4244, Address: "127.0.0.1:7777"}, nil
	}

	out := executeRoot(t, newRootCmd(), "daemon", "restart")

	assert.Equal(t, "restarted pid=4244 address=127.0.0.1:7777\n", string(out))
}

func TestDaemonRestartReservesProcessExitMarginBeyondDrainBudget(t *testing.T) {
	require.Equal(t, 5*time.Second, daemonRestartProcessWaitTimeout-daemonShutdownDrainTimeout)
}

func TestDaemonRestart_JSONReportsStartedDaemon(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	orig := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = orig })
	startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
		return daemonStartOutput{
			Action: "started", PID: 4242, Address: "127.0.0.1:7777",
			WebURL: "http://127.0.0.1:28888",
		}, nil
	}

	out := executeRoot(t, newRootCmd(), "--json", "daemon", "restart")

	var got struct {
		KataAPIVersion int    `json:"kata_api_version"`
		Action         string `json:"action"`
		Stopped        int    `json:"stopped"`
		PIDs           []int  `json:"pids"`
		PID            int    `json:"pid"`
		Address        string `json:"address"`
		WebURL         string `json:"web_url"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "restart", got.Action)
	assert.Zero(t, got.Stopped)
	assert.Empty(t, got.PIDs)
	assert.Equal(t, 4242, got.PID)
	assert.Equal(t, "127.0.0.1:7777", got.Address)
	assert.Equal(t, "http://127.0.0.1:28888", got.WebURL)
}

func TestDaemonRestart_AgentReportsStartedDaemon(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	orig := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = orig })
	startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
		return daemonStartOutput{
			Action: "started", PID: 4242, Address: "127.0.0.1:7777",
			WebURL: "http://127.0.0.1:28888",
		}, nil
	}

	out := executeRoot(t, newRootCmd(), "--agent", "daemon", "restart")

	assert.Equal(t, "OK daemon action=restart pid=4242 stopped=0 "+
		"web_url=http://127.0.0.1:28888\n", string(out))
}

func TestDaemonRestart_PassesStartupOverrides(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)

	orig := startDetachedDaemon
	t.Cleanup(func() { startDetachedDaemon = orig })
	var gotListen string
	var gotInsecureReadonly bool
	startDetachedDaemon = func(_ context.Context, listen string, insecureReadonly bool) (daemonStartOutput, error) {
		gotListen = listen
		gotInsecureReadonly = insecureReadonly
		return daemonStartOutput{Action: "started", PID: 4242, Address: listen}, nil
	}

	executeRoot(t, newRootCmd(), "daemon", "restart", "--listen", "100.64.0.5:7777", "--insecure-readonly")

	assert.Equal(t, "100.64.0.5:7777", gotListen)
	assert.True(t, gotInsecureReadonly)
}

func TestDaemonRestart_ValidatesReplacementBeforeStopping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test helper does not install the Windows daemon stop event watcher")
	}
	tests := []struct {
		name    string
		setup   func(*testing.T, string)
		wantErr string
	}{
		{
			name: "auth config",
			setup: func(t *testing.T, tmp string) {
				require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.toml"),
					[]byte("listen = \"100.64.0.5:7777\"\n"), 0o600))
			},
			wantErr: "pass --insecure-readonly",
		},
		{
			name: "storage DSN",
			setup: func(t *testing.T, _ string) {
				t.Setenv("KATA_DSN", "mysql://db.example/kata")
			},
			wantErr: "unsupported dsn scheme",
		},
		{
			name: "hooks config",
			setup: func(t *testing.T, tmp string) {
				require.NoError(t, os.WriteFile(filepath.Join(tmp, "hooks.toml"),
					[]byte("[[hook]\nevent =\n"), 0o600))
			},
			wantErr: "parse hooks config",
		},
		{
			name: "embedding config",
			setup: func(t *testing.T, tmp string) {
				require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.toml"), []byte(`
[search.embeddings]
base_url = "http://embedding.example"
model = "example-model"
`), 0o600))
			},
			wantErr: "embedding client",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			tmp := setupKataEnv(t)

			child := exec.Command(os.Args[0], "-test.run=TestDaemonCommandSleepHelperProcess", "--") //nolint:gosec // test helper starts this test binary
			child.Env = append(os.Environ(), "KATA_DAEMON_CMD_SLEEP_HELPER=1")
			require.NoError(t, child.Start())
			exited := make(chan struct{})
			go func() {
				_ = child.Wait()
				close(exited)
			}()
			t.Cleanup(func() {
				_ = child.Process.Kill()
				<-exited
			})
			writeRuntimePID(t, tmp, child.Process.Pid)
			tt.setup(t, tmp)

			orig := startDetachedDaemon
			t.Cleanup(func() { startDetachedDaemon = orig })
			startCalled := false
			startDetachedDaemon = func(context.Context, string, bool) (daemonStartOutput, error) {
				startCalled = true
				return daemonStartOutput{}, errors.New("replacement startup attempted")
			}

			_, _, err := executeRootCapture(t, context.Background(), "daemon", "restart")

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.False(t, startCalled)
			assert.True(t, kitdaemon.ProcessAlive(child.Process.Pid), "invalid replacement config must leave the daemon running")
		})
	}
}

func TestValidateFederationStartupConfigActors(t *testing.T) {
	tests := []struct {
		name    string
		actor   string
		wantErr string
	}{
		{name: "valid", actor: "user-a"},
		{name: "empty", actor: "", wantErr: "actor must be non-empty"},
		{name: "reserved", actor: "BOOTSTRAP", wantErr: `actor "bootstrap" is reserved`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.DaemonConfig{
				Federation: config.FederationConfig{
					Projects: []config.FederationProjectConfig{{Actor: tt.actor}},
				},
			}

			err := validateFederationStartupConfig(cfg)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "federation.project[0].actor")
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestDaemonFederationConfigUnavailableHubDoesNotDelayReadiness(t *testing.T) {
	requestStarted := make(chan struct{})
	var requestOnce sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestOnce.Do(func() { close(requestStarted) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(hub.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	baseURL := startDaemonWithFederationConfig(t, federationDaemonConfig(
		hub.URL, `token = "catalog-bearer"`,
	))
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("federation config reconciliation did not begin")
	}

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(baseURL + "/api/v1/ping") //nolint:noctx // bounded loopback test request
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var ping struct {
		OK bool `json:"ok"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ping))
	assert.True(t, ping.OK)

	health := readDaemonFederationHealth(t, baseURL)
	releaseOnce.Do(func() { close(release) })
	assert.True(t, health.OK)
	require.NotNil(t, health.FederationConfig)
	assert.Equal(t, 1, health.FederationConfig.Configured)
	assert.Equal(t, 1, health.FederationConfig.Pending)
	require.NotNil(t, health.FederationConfig.LastAttemptAt)
}

const daemonFederationHubProjectUID = "01HZNQ7VFPK1XGD8R5MABCD4EZ"

func TestDaemonFederationConfigRetriesWhenHubAppearsAndConverges(t *testing.T) {
	address := unusedLoopbackAddress(t)
	var lateServer *http.Server
	t.Cleanup(func() {
		if lateServer != nil {
			_ = lateServer.Close()
		}
	})
	baseURL := startDaemonWithFederationConfig(t, federationDaemonConfig(
		"http://"+address, `token = "catalog-bearer"`,
	))

	require.Eventually(t, func() bool {
		health := readDaemonFederationHealth(t, baseURL)
		return health.FederationConfig != nil &&
			health.FederationConfig.LastErrorCategory == "hub_unavailable"
	}, 3*time.Second, 20*time.Millisecond)

	var catalogRequests atomic.Int32
	listener, err := net.Listen("tcp", address)
	require.NoError(t, err)
	lateServer = &http.Server{ //nolint:gosec // loopback-only test server
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer catalog-bearer" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			catalogRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/projects/resolve":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"project": map[string]any{
						"id": 42, "uid": daemonFederationHubProjectUID, "name": "hub-project",
					},
				})
			case "/api/v1/projects/42/federation/enable":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"project_id": 42, "project_uid": daemonFederationHubProjectUID,
					"project_name": "hub-project", "replay_horizon_event_id": 9,
					"baseline_through_event_id": 6,
				})
			case "/api/v1/federation/enrollments":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 71, "actor": "identity-user",
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}),
	}
	go func() {
		_ = lateServer.Serve(listener)
	}()

	require.Eventually(t, func() bool {
		health := readDaemonFederationHealth(t, baseURL)
		return health.OK &&
			health.FederationConfig != nil &&
			health.FederationConfig.Reconciled == 1 &&
			health.FederationConfig.Pending == 0 &&
			health.FederationConfig.Conflicted == 0
	}, 5*time.Second, 20*time.Millisecond)
	assert.Positive(t, catalogRequests.Load())
}

func TestDaemonFederationConfigEmptyTokenEnvFailsOpenWithoutGlobalBearer(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "daemon-global-bearer")
	t.Setenv("KATA_TEST_EMPTY_HUB_TOKEN", "")
	var requests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(hub.Close)

	baseURL := startDaemonWithFederationConfig(t, federationDaemonConfig(
		hub.URL, `token_env = "KATA_TEST_EMPTY_HUB_TOKEN"`,
	))
	require.Eventually(t, func() bool {
		health := readDaemonFederationHealth(t, baseURL)
		return health.OK &&
			health.FederationConfig != nil &&
			health.FederationConfig.Pending == 1 &&
			health.FederationConfig.LastErrorCategory == "hub_authentication"
	}, 3*time.Second, 20*time.Millisecond)
	assert.Zero(t, requests.Load(), "empty catalog token_env must fail before sending any bearer")
}

func TestDaemonFederationConfigNoMappingsSkipsReconcilerFactory(t *testing.T) {
	var factoryCalls atomic.Int32
	original := newFederationConfigReconciler
	newFederationConfigReconciler = func(
		cfg federation.ReconcilerConfig,
	) federationConfigReconciler {
		factoryCalls.Add(1)
		return original(cfg)
	}
	t.Cleanup(func() { newFederationConfigReconciler = original })

	baseURL := startDaemonWithFederationConfig(t, "")
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(baseURL + "/api/v1/ping") //nolint:noctx // bounded loopback test request
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Zero(t, factoryCalls.Load())
}

func TestDaemonWiresNamedCatalogToFederationRebindRoute(t *testing.T) {
	baseURL := startDaemonWithFederationConfig(t, `
[[daemon]]
name = "primary-hub"
url = "https://hub.example"
`)
	requestBody := strings.NewReader(`{"hub_catalog":"primary-hub"}`)
	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost,
		baseURL+"/api/v1/federation/replicas/999/actions/rebind", requestBody,
	)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusNotFound, response.StatusCode, string(body))
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	assert.Equal(t, "federation_project_not_found", envelope.Error.Code, string(body))
}

type daemonFederationHealth struct {
	OK               bool                        `json:"ok"`
	FederationConfig *api.FederationConfigHealth `json:"federation_config"`
}

func startDaemonWithFederationConfig(t *testing.T, configBody string) string {
	t.Helper()
	resetFlags(t)
	home := setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemon.AutoStartMarkerEnv, "1")
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"), []byte(configBody), 0o600,
	))

	originalTelemetry := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = originalTelemetry })

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

	namespace, err := daemon.NewNamespace()
	require.NoError(t, err)
	runtimePath, err := (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).Path(os.Getpid())
	require.NoError(t, err)
	var record daemonRuntimeRecordJSON
	require.Eventually(t, func() bool {
		body, readErr := os.ReadFile(runtimePath) //nolint:gosec // test-owned KATA_HOME
		return readErr == nil && json.Unmarshal(body, &record) == nil
	}, 3*time.Second, 10*time.Millisecond)
	return "http://" + record.Address
}

func readDaemonFederationHealth(t *testing.T, baseURL string) daemonFederationHealth {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(baseURL + "/api/v1/health") //nolint:noctx // bounded loopback test request
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var health daemonFederationHealth
	require.NoError(t, json.Unmarshal(body, &health), string(body))
	return health
}

func federationDaemonConfig(hubURL, authLine string) string {
	return fmt.Sprintf(`
[[daemon]]
name = "primary"
url = %q
%s
allow_insecure = true

[[federation.project]]
hub = "primary"
spoke_project = "spoke-project"
hub_project = "hub-project"
actor = "user-a"
`, hubURL, authLine)
}

func unusedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
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

func TestAutostartIdleControllerRequiresMarkerAndOwnerLocalExposure(t *testing.T) {
	localDaemon := kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/kata.sock"}
	localWeb := daemon.WebEndpoint{
		Endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:27123"},
		Origin:   "http://127.0.0.1:27123",
	}
	configured := config.DaemonConfig{AutostartIdleTimeout: "15m"}

	controller, ineligible, err := newAutostartIdleController(
		configured, false, localDaemon, localWeb, func(time.Duration) {},
	)
	require.NoError(t, err)
	require.Nil(t, controller)
	require.False(t, ineligible)

	controller, ineligible, err = newAutostartIdleController(
		configured, true,
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "100.64.0.5:7777"},
		localWeb, func(time.Duration) {},
	)
	require.NoError(t, err)
	require.Nil(t, controller)
	require.True(t, ineligible)

	controller, ineligible, err = newAutostartIdleController(
		configured, true, localDaemon, localWeb, func(time.Duration) {},
	)
	require.NoError(t, err)
	require.NotNil(t, controller)
	require.False(t, ineligible)
	require.Equal(t, 15*time.Minute, controller.Snapshot().Timeout)
}

func TestAnnounceIdleShutdownNamesTheTimeout(t *testing.T) {
	var out bytes.Buffer

	announceIdleShutdown(&out, 15*time.Minute)

	assert.Equal(t, "kata daemon: idle shutdown after 15m0s without client activity\n", out.String())
}

func TestWithoutEnvironmentKeyRemovesOnlyAutostartMarker(t *testing.T) {
	got := withoutEnvironmentKey([]string{
		"PATH=/example/bin",
		daemon.AutoStartMarkerEnv + "=1",
		"kata_autostart=1",
		"Kata_Autostart=1",
		"KATA_AUTOSTART_IDLE_TIMEOUT=15m",
	}, daemon.AutoStartMarkerEnv)

	assert.Equal(t, []string{
		"PATH=/example/bin",
		"KATA_AUTOSTART_IDLE_TIMEOUT=15m",
	}, got)
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

func TestVectorsPathForDSN(t *testing.T) {
	// Derived from the database filename so two SQLite databases in one
	// directory never share sidecar state.
	got, err := vectorsPathForDSN("/var/lib/kata/kata.db")
	if err != nil || got != "/var/lib/kata/kata.vectors.db" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = vectorsPathForDSN("sqlite:///var/lib/kata/kata.db")
	if err != nil || got != "/var/lib/kata/kata.vectors.db" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = vectorsPathForDSN("/var/lib/kata/other.db")
	if err != nil || got != "/var/lib/kata/other.vectors.db" {
		t.Fatalf("got %q, %v", got, err)
	}
	// A path without a .db suffix gets .vectors appended, so it can never
	// collide with the .db-suffixed form of the same stem: data and data.db
	// in one directory must map to distinct sidecars.
	got, err = vectorsPathForDSN("/var/lib/kata/data")
	if err != nil || got != "/var/lib/kata/data.vectors" {
		t.Fatalf("got %q, %v", got, err)
	}
	withDB, err := vectorsPathForDSN("/var/lib/kata/data.db")
	if err != nil || withDB == got {
		t.Fatalf("data and data.db must not share a sidecar: %q vs %q (%v)", got, withDB, err)
	}
	got, err = vectorsPathForDSN("postgres://h/db")
	if err != nil || got != "" {
		t.Fatalf("postgres uses canonical pgvector tables, got %q, %v", got, err)
	}
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
	wake := startGitHubSyncRunner(ctx, newDaemonWorkerGroup(), nil, store, fetcher, bcast, hooks.NewNoop(), log.New(io.Discard, "", 0))
	defer cancel()

	require.Eventually(t, func() bool {
		return runner.wasRun()
	}, time.Second, time.Millisecond)
	require.Len(t, configs, 1)
	assert.Same(t, store, configs[0].Store)
	assert.Same(t, fetcher, configs[0].Fetcher)
	assert.Equal(t, 25*time.Millisecond, configs[0].Interval)
	assert.NotNil(t, configs[0].Wake)
	assert.NotNil(t, configs[0].EventSinkFrom)
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
	startGitHubSyncRunner(ctx, newDaemonWorkerGroup(), nil, store, nil, daemon.NewEventBroadcaster(), hooks.NewNoop(), log.New(io.Discard, "", 0))

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
	startGitHubSyncRunner(ctx, newDaemonWorkerGroup(), nil, store, fetcher, bcast, hooks.NewNoop(), log.New(io.Discard, "", 0))

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
	startGitHubSyncRunner(ctx, newDaemonWorkerGroup(), nil, store, fetcher, bcast, hookSink, log.New(io.Discard, "", 0))

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
	wake := startGitHubSyncRunner(ctx, newDaemonWorkerGroup(), nil, store, fetcher, daemon.NewEventBroadcaster(), hooks.NewNoop(), log.New(io.Discard, "", 0))

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

func TestPreflightDaemonStartupWebConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "config.toml"), []byte(`
[web]
listen = "127.0.0.1:27123"
public_origin = "https://daemon.example"
`), 0o600))

	startup, err := preflightDaemonStartup(context.Background(), "", false)
	require.NoError(t, err)
	assert.Equal(t, config.WebConfig{
		Listen:       "127.0.0.1:27123",
		PublicOrigin: "https://daemon.example",
	}, startup.Web)
}

func TestDaemonRuntimeWebMetadata(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv(daemon.AutoStartMarkerEnv, "1")

	originalTelemetry := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = originalTelemetry })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemonWithListen(ctx, "127.0.0.1:0", false) }()

	namespace, err := daemon.NewNamespace()
	require.NoError(t, err)
	runtimePath, err := (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).Path(os.Getpid())
	require.NoError(t, err)
	var record kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		body, readErr := os.ReadFile(runtimePath) //nolint:gosec // test-owned KATA_HOME
		return readErr == nil && json.Unmarshal(body, &record) == nil &&
			record.Metadata["web_origin"] != ""
	}, 3*time.Second, 10*time.Millisecond)

	assert.NotEmpty(t, record.Metadata["web_origin"])
	assert.Equal(t, "true", record.Metadata["web_origin_stable"])
	assert.Equal(t, "loopback,sse", record.Metadata["web_capabilities"])
	if runtime.GOOS != "windows" {
		runtimeStat, statErr := os.Stat(runtimePath)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o644), runtimeStat.Mode().Perm())
	}

	cancel()
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("daemon did not stop cleanly: %v", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop after context cancellation")
	}
}

func TestDaemonRuntimeWebMetadata_TokenProtectedReadonlyRequiresLogin(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("PORT", "")
	t.Setenv("KATA_AUTH_TOKEN", "example-token")
	t.Setenv(daemon.AutoStartMarkerEnv, "1")

	originalTelemetry := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		return &fakeTelemetryReporter{}
	}
	t.Cleanup(func() { newTelemetryReporter = originalTelemetry })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemonWithListen(ctx, "127.0.0.1:0", true) }()

	namespace, err := daemon.NewNamespace()
	require.NoError(t, err)
	runtimePath, err := (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).Path(os.Getpid())
	require.NoError(t, err)
	var record kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		body, readErr := os.ReadFile(runtimePath) //nolint:gosec // test-owned KATA_HOME
		return readErr == nil && json.Unmarshal(body, &record) == nil &&
			record.Metadata["web_origin"] != ""
	}, 3*time.Second, 10*time.Millisecond)

	assert.Equal(t, "login,poll", record.Metadata["web_capabilities"])

	cancel()
	select {
	case runErr := <-done:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("daemon did not stop cleanly: %v", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not stop after context cancellation")
	}
}

func TestWebAuthenticationModeAdvertisesOnlyUsableSessionIssuers(t *testing.T) {
	for _, tc := range []struct {
		name             string
		insecureReadonly bool
		allowLocal       bool
		allowProxy       bool
		token            string
		want             string
	}{
		{name: "direct loopback", allowLocal: true, want: "loopback"},
		{name: "trusted proxy", allowProxy: true, want: "proxy"},
		{name: "daemon token", token: "example-token", want: "login"},
		{name: "anonymous readonly", insecureReadonly: true, want: "readonly"},
		{name: "no session issuer", want: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, webAuthenticationMode(
				tc.insecureReadonly, tc.allowLocal, tc.allowProxy, tc.token,
			))
		})
	}
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
	if rawDelay := os.Getenv("KATA_DAEMON_CMD_SHUTDOWN_DELAY"); rawDelay != "" {
		delay, err := time.ParseDuration(rawDelay)
		if err != nil {
			os.Exit(2)
		}
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown)
		if readyPath := os.Getenv("KATA_DAEMON_CMD_READY_PATH"); readyPath != "" {
			if err := os.WriteFile(readyPath, nil, 0o600); err != nil { //nolint:gosec // test-controlled path under t.TempDir
				os.Exit(3)
			}
		}
		<-shutdown
		time.Sleep(delay)
		os.Exit(0)
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

func writeReusedRuntimePID(t *testing.T, home string, pid int) {
	t.Helper()
	identity, ok := kitdaemon.ReadProcessIdentity(pid)
	require.True(t, ok)
	encoded := string(identity)
	replacement := byte('0')
	if encoded[len(encoded)-1] == replacement {
		replacement = '1'
	}
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:               pid,
		ProcessIdentity:   identity,
		ProcessIdentityV2: kitdaemon.ProcessIdentity(encoded[:len(encoded)-1] + string(replacement)),
		Network:           "unix",
		Address:           filepath.Join(home, "daemon.sock"),
		StartedAt:         time.Now().UTC(),
	})
	require.NoError(t, err)
}

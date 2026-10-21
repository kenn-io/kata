package daemon_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	kitdaemon "go.kenn.io/kit/daemon"
)

// relayProcessEnvironment deliberately excludes inherited Kata targets,
// credentials, proxies, hosted PORT, and agent hooks for every child.
func relayProcessEnvironment(home string) []string {
	var environment []string
	for _, name := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT", "GOTOOLCHAIN", "CGO_ENABLED", "CGO_CFLAGS", "CGO_LDFLAGS"} {
		if value, found := os.LookupEnv(name); found {
			environment = append(environment, name+"="+value)
		}
	}
	return append(environment, "HOME="+home, "USERPROFILE="+home,
		"KATA_HOME="+home, "GOMAXPROCS=2", "GOFLAGS=-p=2",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"))
}

func relayProcessHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "kra-relay-process-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(home)) })
	return home
}

func relayProcessToken(t *testing.T) string {
	t.Helper()
	var bytes [32]byte
	_, err := rand.Read(bytes[:])
	require.NoError(t, err)
	return hex.EncodeToString(bytes[:])
}

func buildRelayProcessCLI(t *testing.T) string {
	t.Helper()
	// Resolve installed caches before isolating HOME, like testfix.RunIsolated.
	probe := exec.CommandContext(t.Context(), "go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT")
	probe.Env = relayProcessEnvironment(os.Getenv("HOME"))
	resolved, err := probe.Output()
	require.NoError(t, err)
	var toolchain map[string]string
	require.NoError(t, json.Unmarshal(resolved, &toolchain))
	home := relayProcessHome(t)
	binary := filepath.Join(home, "kata")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	// Build with the selected toolchain but an isolated user home as well.
	//nolint:gosec // Fixed build arguments and a test-owned executable.
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "kit_posthog_disabled", "-buildvcs=false", "-o", binary, "go.kenn.io/kata/cmd/kata")
	build.Env = relayProcessEnvironment(home)
	for name, value := range toolchain {
		build.Env = append(build.Env, name+"="+value)
	}
	output, err := build.CombinedOutput()
	require.NoError(t, err, "compile branch CLI: %s", output)
	return binary
}

type relayProcess struct {
	t          *testing.T
	binary     string
	home       string
	ownerToken string
	url        string
	pid        int
	exited     chan struct{}
	client     *http.Client
	history    db.Storage
}

func startRelayProcess(t *testing.T, binary, home string) *relayProcess {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	log, err := os.Create(filepath.Join(home, "process.log")) //nolint:gosec // home is a test-owned temporary directory.
	require.NoError(t, err)
	//nolint:gosec // Runs only the compiled branch CLI in a scratch home.
	cmd := exec.Command(binary, "daemon", "start", "--foreground", "--listen", address, "--no-auto-token")
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = home, relayProcessEnvironment(home), log, log
	cmd.Env = append(cmd.Env, "KATA_FEDERATION_PULL_INTERVAL_MS=250")
	if err := cmd.Start(); err != nil {
		if closeErr := log.Close(); closeErr != nil {
			t.Fatalf("start scratch daemon: %v; close process log: %v", err, closeErr)
		}
		t.Fatalf("start scratch daemon: %v", err)
	}
	p := &relayProcess{t: t, binary: binary, home: home, url: "http://" + address,
		pid: cmd.Process.Pid, exited: make(chan struct{}),
		client: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	go func() { _ = cmd.Wait(); close(p.exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-p.exited:
		case <-time.After(5 * time.Second):
			t.Error("scratch daemon did not exit")
		}
		p.client.CloseIdleConnections()
		require.NoError(t, log.Close())
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Logf("scratch daemon log: %s", data)
		}
	})
	//nolint:kennlint // Observe readiness of the actual operating-system process.
	require.Eventually(t, p.healthy, 15*time.Second, 50*time.Millisecond)
	return p
}

func (p *relayProcess) healthy() bool {
	select {
	case <-p.exited:
		return false
	default:
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, p.url+"/api/v1/health", nil)
	if err != nil {
		return false
	}
	response, err := p.client.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return false
	}
	directories, err := filepath.Glob(filepath.Join(p.home, "runtime", "*"))
	if err != nil {
		return false
	}
	for _, directory := range directories {
		records, err := (kitdaemon.RuntimeStore{Dir: directory}).List()
		if err != nil {
			return false
		}
		for _, record := range records {
			if record.PID == p.pid {
				return true
			}
		}
	}
	return false
}

func (p *relayProcess) cli(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(p.t.Context(), 20*time.Second)
	defer cancel()
	//nolint:gosec // Runs the compiled branch CLI with test-owned arguments.
	command := exec.CommandContext(ctx, p.binary, args...)
	command.Dir = p.home
	command.Env = append(relayProcessEnvironment(p.home), "KATA_SERVER="+p.url)
	if p.ownerToken != "" {
		command.Env = append(command.Env, "KATA_AUTH_TOKEN="+p.ownerToken)
	}
	return command.CombinedOutput()
}

func (p *relayProcess) get(path, token string) (int, []byte) {
	return p.request(http.MethodGet, path, token, nil)
}

func (p *relayProcess) request(method, path, token string, body any) (int, []byte) {
	p.t.Helper()
	var contents []byte
	var err error
	if body != nil {
		contents, err = json.Marshal(body)
		require.NoError(p.t, err)
	}
	request, err := http.NewRequestWithContext(p.t.Context(), method, p.url+path, bytes.NewReader(contents))
	require.NoError(p.t, err)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := p.client.Do(request)
	require.NoError(p.t, err)
	defer func() { _ = response.Body.Close() }()
	contents, err = io.ReadAll(io.LimitReader(response.Body, 64<<20))
	require.NoError(p.t, err)
	return response.StatusCode, contents
}

// monitorServing observes the same process throughout publication, baseline
// and subsequent writes. It never calls testing assertions from its goroutine.
func (p *relayProcess) monitorServing(t *testing.T) {
	t.Helper()
	stop, done := make(chan struct{}), make(chan struct{})
	var failures int
	t.Cleanup(func() {
		close(stop)
		<-done
		require.Zero(t, failures, "hub must serve health continuously without replacing its original process")
		require.True(t, p.healthy())
	})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !p.healthy() {
					failures++
				}
			}
		}
	}()
}

// Wait in the test goroutine so failed public requests retain their ordinary
// assertion semantics. The deadline bounds retries of asynchronously synced data.
func awaitRelayProcess(t *testing.T, label string, ready func() error) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := ready()
		if err == nil {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("%s: %v", label, t.Context().Err())
		case <-deadline.C:
			t.Fatalf("%s: %v", label, err)
		case <-ticker.C:
		}
	}
}

func writeRelayProcessConfig(t *testing.T, home, contents string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0600))
}

func TestRelayProcessHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs an isolated branch daemon")
	}
	// A hostile inherited target must never affect the compiled CLI or daemon.
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	t.Setenv("KATA_AUTH_TOKEN", "unrelated-inherited-credential")
	t.Setenv("PORT", "1")
	process := startRelayProcess(t, buildRelayProcessCLI(t), relayProcessHome(t))
	output, err := process.cli("federation", "identity", "--json")
	require.NoError(t, err, "%s", output)
	require.Contains(t, string(output), "instance_uid")
	require.True(t, process.healthy())
	status, _ := process.get("/api/v1/instance", "")
	require.Equal(t, http.StatusOK, status)
}

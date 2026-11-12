package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/selfupdate"
)

func copyTestExecutable(t *testing.T, srcPath, dstPath string) {
	t.Helper()
	src, err := os.Open(srcPath) //nolint:gosec // G304: srcPath is a test-built binary in a temporary directory.
	require.NoError(t, err)
	defer func() {
		if err := src.Close(); err != nil {
			t.Errorf("close source binary: %v", err)
		}
	}()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // G304: dstPath is a test-owned temporary executable.
	require.NoError(t, err)
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	require.NoError(t, copyErr)
	require.NoError(t, closeErr)
}

type daemonLifecycleFixture struct {
	binary, cli, home string
	env               []string
	store             kitdaemon.RuntimeStore
	record            func() kitdaemon.RuntimeRecord
}

func daemonLifecycle(t *testing.T) func(*testing.T, string) *daemonLifecycleFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs isolated daemon binaries")
	}
	// Build before the child environment clears GOFLAGS, so these builds reuse
	// the package cache and run from the module directory.
	moduleDir, err := os.Getwd()
	require.NoError(t, err)
	source := filepath.Join(moduleDir, "..", "..", "internal", "config", "daemon_config.go")
	contents, err := os.ReadFile(source) //nolint:gosec // test-owned source overlay
	require.NoError(t, err)
	overlaySource := filepath.Join(t.TempDir(), "daemon_config.go")
	contents = bytes.Replace(contents, []byte("type DaemonConfig struct {"), []byte("type DaemonConfig struct {\n FutureSetting string `toml:\"future_setting\"`"), 1)
	require.NoError(t, os.WriteFile(overlaySource, contents, 0600)) //nolint:gosec // G703: source overlay lives in a test-owned temporary directory
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: overlaySource}})
	require.NoError(t, err)
	overlayPath := filepath.Join(t.TempDir(), "overlay.json")
	require.NoError(t, os.WriteFile(overlayPath, overlay, 0600))
	binaries := map[string]string{
		"v0.1.0": kataTestBinary(t, "v0.1.0"),
		"v0.2.0": testBinary(t, "kata-v0.2.0-future-setting", "-buildvcs=false", "-overlay="+overlayPath, "-ldflags", "-X go.kenn.io/kata/internal/version.Version=v0.2.0", "go.kenn.io/kata/cmd/kata"),
	}
	childEnv := daemonLifecycleEnvironment(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	return func(t *testing.T, scenario string) *daemonLifecycleFixture {
		t.Helper()
		resetFlags(t)
		home, err := config.KataHome()
		require.NoError(t, err)
		binary := filepath.Join(t.TempDir(), "kata")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		// This is a fresh test path, so a direct copy is enough. Keep the
		// staged replacement exercised by fake.install below, where the
		// existing daemon binary really must be replaced.
		copyTestExecutable(t, binaries["v0.1.0"], binary)
		env := append(append([]string(nil), childEnv...), "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"), "KATA_WORKSPACE="+workspace, "KATA_AUTHOR=user-a")
		if strings.Contains(scenario, "during drain") {
			shell, err := exec.LookPath("sh")
			require.NoError(t, err)
			command, err := json.Marshal(shell)
			require.NoError(t, err)
			args, err := json.Marshal([]string{"-c", `touch "$1"; while [ ! -f "$2" ]; do sleep 0.01; done`, "hook", filepath.Join(home, "hook-ready"), filepath.Join(home, "hook-release")})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(home, "hooks.toml"), []byte("[[hook]]\nevent = 'issue.created'\ncommand = "+string(command)+"\nargs = "+string(args)+"\ntimeout = '10s'\n"), 0600))
		}
		if scenario == "startup flags" {
			env = append(env, "KATA_TRUST_PRIVATE_NETWORK=1")
		}
		// The daemon restarts from its own environment. A hosted PORT in the
		// updater's shell must not move a Unix-socket daemon onto wildcard TCP.
		t.Setenv("PORT", "4321")
		if scenario == "daemon token" {
			t.Setenv("KATA_AUTH_TOKEN", "caller-token")
		}
		if scenario == "daemon token" || scenario == "stop waits for held request" || strings.Contains(scenario, "during drain") {
			env = append(env, "KATA_AUTH_TOKEN=fixture-token")
		}
		if scenario == "idle shutdown" {
			env = append(env, "KATA_AUTOSTART=1", "KATA_AUTOSTART_IDLE_TIMEOUT=10s")
		}
		ns, err := daemonruntime.NewNamespace()
		require.NoError(t, err)
		store := kitdaemon.RuntimeStore{Dir: ns.DataDir}
		record := func() kitdaemon.RuntimeRecord {
			records, err := store.List()
			require.NoError(t, err)
			for _, rec := range records {
				if daemonruntime.RuntimeProcessAlive(rec) {
					return rec
				}
			}
			return kitdaemon.RuntimeRecord{}
		}
		t.Cleanup(func() {
			// t.Context() is already canceled when cleanup runs.
			stop := exec.CommandContext(context.WithoutCancel(t.Context()), binaries["v0.2.0"], "daemon", "stop") //nolint:gosec // test-owned binary
			stop.Env = env
			output, err := stop.CombinedOutput()
			assert.NoError(t, err, "%s", output)
			require.Eventually(t, func() bool { return record().PID == 0 }, 10*time.Second, 20*time.Millisecond) //nolint:kennlint // sleeptest: external daemon removal is observed through its runtime record
		})
		if scenario != "stopped" {
			args := []string{"daemon", "start", "--foreground"}
			switch scenario {
			case "startup flags":
				args = append(args, "--")
			case "read-only TCP":
				args = append(args, "--listen", "127.0.0.1:0", "--insecure-readonly")
			case "daemon token", "older daemon", "restart during drain", "start during drain", "signal-only single installation", "signal-only second installation":
				args = append(args, "--listen", "127.0.0.1:0")
			}
			// t.Cleanup owns this process; t.Context() would kill it before that runs.
			process := exec.CommandContext(context.WithoutCancel(t.Context()), binary, args...) //nolint:gosec // test-owned binary and fixed arguments
			process.Env = env
			process.Stderr = os.Stderr
			require.NoError(t, process.Start())
			exited := make(chan error, 1)
			go func() { exited <- process.Wait() }()
			t.Cleanup(func() {
				_ = process.Process.Kill()
				select {
				case <-exited:
				case <-time.After(10 * time.Second):
					t.Error("original daemon did not exit")
				}
			})
			require.Eventually(t, func() bool { return record().PID != 0 }, 10*time.Second, 20*time.Millisecond) //nolint:kennlint // sleeptest: external daemon readiness is published through its runtime record
		}
		return &daemonLifecycleFixture{binary: binary, cli: binaries["v0.2.0"], home: home, env: env, store: store, record: record}
	}
}

func (f *daemonLifecycleFixture) restart(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), f.cli, append([]string{"daemon", "restart"}, args...)...) //nolint:gosec // test-built executable
	command.Env = append(append([]string(nil), f.env...), "PORT=4321")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		output = append(output, stderr.Bytes()...)
	}
	return string(output), err
}

func TestDaemonLifecycle(t *testing.T) {
	fixture := daemonLifecycle(t)
	t.Run("update", func(t *testing.T) { testUpdateInstallDaemonLifecycle(t, fixture) })
	t.Run("restart", func(t *testing.T) { testDaemonRestartLifecycle(t, fixture) })
	t.Run("stop waits for held request", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("holds an HTTP request over a Unix socket")
		}
		setupKataEnv(t)
		f := fixture(t, "stop waits for held request")
		before := f.record()
		socket := strings.TrimPrefix(before.Address, "unix://")
		dialer := net.Dialer{Timeout: time.Second}
		conn, err := dialer.DialContext(t.Context(), "unix", socket)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
		body := `{"name":"example/recovery","actor":"user-a"}`
		_, err = fmt.Fprintf(conn, "POST /api/v1/projects HTTP/1.1\r\nHost: example.test\r\nAuthorization: Bearer fixture-token\r\nContent-Type: application/json\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", len(body))
		require.NoError(t, err)
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusContinue, response.StatusCode)
		require.NoError(t, response.Body.Close())
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		stop := exec.CommandContext(ctx, f.cli, "daemon", "stop") //nolint:gosec // test-built executable
		stop.Env = f.env
		stopped := make(chan struct{})
		var stopOutput []byte
		var stopErr error
		go func() { stopOutput, stopErr = stop.CombinedOutput(); close(stopped) }()
		t.Cleanup(func() { cancel(); <-stopped })
		require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: listener closure proves the actual daemon is draining the held request
			probe, err := dialer.DialContext(t.Context(), "unix", socket)
			if err != nil {
				return true
			}
			_ = probe.Close()
			return false
		}, 5*time.Second, 20*time.Millisecond)
		select {
		case <-stopped:
			t.Fatalf("stop returned while HTTP request was still active: %s (%v)", stopOutput, stopErr)
		case <-time.After(200 * time.Millisecond): //nolint:kennlint // deadlinetest: stop must remain pending while the request body is withheld
		}
		_, err = io.WriteString(conn, body)
		require.NoError(t, err)
		response, err = http.ReadResponse(reader, nil)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		<-stopped
		require.NoError(t, stopErr, "%s", stopOutput)
	})
}

func testUpdateInstallDaemonLifecycle(t *testing.T, fixture func(*testing.T, string) *daemonLifecycleFixture) {
	t.Helper()
	for _, scenario := range []string{"running", "second installation", "renamed updater", "signal-only single installation", "signal-only second installation", "stopped", "check only", "cancelled", "install failure", "read-only TCP", "older daemon", "daemon token", "idle shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{CurrentVersion: "v0.1.0", LatestVersion: "v0.2.0"}}}
			stubUpdateClient(t, fake)
			f := fixture(t, scenario)
			before := f.record()
			installed := f.binary
			if scenario == "second installation" || scenario == "signal-only second installation" {
				installed = filepath.Join(t.TempDir(), filepath.Base(f.binary))
				copyTestExecutable(t, f.binary, installed)
			}
			updater := installed
			if scenario == "renamed updater" {
				updater = filepath.Join(filepath.Dir(installed), "kata-preview"+filepath.Ext(installed))
				copyTestExecutable(t, installed, updater)
			}
			stubDaemonExecutable(t, func() (string, error) { return updater, nil })
			if strings.HasPrefix(scenario, "signal-only") {
				require.True(t, daemonruntime.RuntimeRecordRestartable(before))
				delete(before.Metadata, daemonruntime.RuntimeRestartExecutableKey)
				_, err := f.store.Write(before)
				require.NoError(t, err)
			}
			if scenario == "older daemon" {
				delete(before.Metadata, daemonruntime.RuntimeRestartMetadataKey)
				delete(before.Metadata, daemonruntime.RuntimeRestartExecutableKey)
				_, err := f.store.Write(before)
				require.NoError(t, err)
			}
			var installOptions selfupdate.InstallOptions
			fake.install = func(opts selfupdate.InstallOptions) error {
				installOptions = opts
				if scenario == "install failure" {
					return errors.New("download failed")
				}
				destination := opts.DestinationPath
				if destination == "" {
					// Kit defaults to the canonical binary name beside the updater.
					destination = installed
				}
				if err := selfupdate.InstallBinary(f.cli, destination); err != nil {
					return err
				}
				return nil
			}
			if scenario == "check only" || scenario == "cancelled" {
				if scenario == "check only" {
					_, _, err := executeRootCapture(t, t.Context(), "update", "--check")
					require.NoError(t, err)
				} else {
					_, _, err := executeRootCaptureWithInput(t.Context(), t, "n\n", "update")
					assert.Equal(t, kindConfirm, requireCLIError(t, err, ExitConfirm).Kind)
				}
				assert.Empty(t, fake.installed)
				assert.Equal(t, before.StartedAt, f.record().StartedAt)
				return
			}
			stdout, stderr, updateErr := executeRootCapture(t, t.Context(), "update", "--yes", "--json")
			if scenario == "signal-only second installation" {
				after := f.record()
				require.True(t, after.StartedAt.After(before.StartedAt), "signal-only daemon must attempt restart")
				require.Equal(t, "v0.1.0", after.Version)
				require.Error(t, updateErr)
				assert.Contains(t, updateErr.Error(), "older kata release")
				assert.NotEmpty(t, fake.installed)
				assert.NotContains(t, stdout, `"installed":true`)
				assert.NotContains(t, stderr, "restarted daemon")
				return
			}
			if scenario == "older daemon" {
				require.Error(t, updateErr)
				assert.Contains(t, updateErr.Error(), "installed kata v0.2.0, but the daemon was not restarted")
				assert.NotEmpty(t, fake.installed)
				assert.Contains(t, updateErr.Error(), "kata daemon stop")
				assert.Equal(t, before.StartedAt, f.record().StartedAt)
				httpClient, baseURL := client.LocalHTTPClient(before.Endpoint().ConfigAddress())
				status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/ping", nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusOK, status)
				return
			}
			if scenario == "install failure" {
				require.Error(t, updateErr)
				assert.Equal(t, before.StartedAt, f.record().StartedAt)
				return
			}
			require.NoError(t, updateErr, "%s", stderr)
			var result struct {
				Installed bool `json:"installed"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &result))
			assert.True(t, result.Installed)
			after := f.record()
			if scenario == "stopped" {
				assert.Zero(t, after.PID)
				assert.NotContains(t, stderr, "restarted")
				return
			}
			require.True(t, after.StartedAt.After(before.StartedAt), "update must replace the running daemon")
			assert.Equal(t, "v0.2.0", after.Version)
			if scenario == "renamed updater" {
				assert.Equal(t, installed, installOptions.DestinationPath)
			}
			if scenario == "second installation" || scenario == "renamed updater" {
				p, err := process.NewProcess(int32(after.PID)) //nolint:gosec // PID belongs to the test-built daemon.
				require.NoError(t, err)
				executable, err := p.Exe()
				require.NoError(t, err)
				assert.Equal(t, installed, executable, "daemon must run the installation that was updated")
			}
			assert.Contains(t, stderr, "restarted daemon")
			httpClient, baseURL := client.LocalHTTPClient(after.Endpoint().ConfigAddress())
			status, _, err := httpDoJSON(t.Context(), httpClient, "GET", baseURL+"/api/v1/ping", nil)
			require.NoError(t, err)
			assert.Equal(t, 200, status)
			switch scenario {
			case "running":
				assert.Equal(t, before.Endpoint().IsUnix(), after.Endpoint().IsUnix())
			case "daemon token":
				status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/projects", nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusUnauthorized, status)
				status, _, err = httpDoJSONHeaders(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/projects", nil, map[string]string{"Authorization": "Bearer fixture-token"})
				require.NoError(t, err)
				assert.Equal(t, http.StatusOK, status)
			case "idle shutdown":
				// The replacement advertises the idle policy it inherited. A health
				// probe does not count as activity, so the deadline stays armed.
				status, body, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/health", nil)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, status)
				var health struct {
					IdleShutdown *struct {
						Timeout  string     `json:"timeout"`
						State    string     `json:"state"`
						Deadline *time.Time `json:"deadline"`
					} `json:"idle_shutdown"`
				}
				require.NoError(t, json.Unmarshal(body, &health))
				require.NotNil(t, health.IdleShutdown, "the replacement must still stop itself when idle: %s", body)
				assert.Equal(t, "10s", health.IdleShutdown.Timeout)
				assert.Equal(t, "armed", health.IdleShutdown.State)
				require.NotNil(t, health.IdleShutdown.Deadline)
				assert.True(t, health.IdleShutdown.Deadline.After(after.StartedAt), "the idle deadline must belong to the replacement")
			case "read-only TCP":
				assert.False(t, after.Endpoint().IsUnix())
				// Anonymous read-only mode must survive the automatic restart.
				status, _, err = httpDoJSON(t.Context(), httpClient, http.MethodPost, baseURL+"/api/v1/projects", map[string]string{"name": "example-project", "actor": "user-a"})
				require.NoError(t, err)
				assert.Equal(t, http.StatusUnauthorized, status)
			}
		})
	}
}

func testDaemonRestartLifecycle(t *testing.T, fixture func(*testing.T, string) *daemonLifecycleFixture) {
	t.Helper()
	for _, scenario := range []string{"restart failure", "replacement fails after exec", "older daemon", "startup flags", "new config", "restart during drain", "start during drain"} {
		t.Run(scenario, func(t *testing.T) {
			if strings.Contains(scenario, "during drain") && runtime.GOOS != "linux" {
				t.Skip("holds drain with a shell hook and observes the start lock through inotify")
			}
			setupKataEnv(t)
			f := fixture(t, scenario)
			before := f.record()
			if scenario != "older daemon" && scenario != "replacement fails after exec" && runtime.GOOS != "windows" {
				t.Cleanup(func() { assert.Equal(t, before.PID, f.record().PID) })
			}
			if strings.Contains(scenario, "during drain") {
				release := filepath.Join(f.home, "hook-release")
				t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
				for _, args := range [][]string{{"init", "--project", "example/project"}, {"create", "hold restart drain", "--json"}} {
					command := exec.CommandContext(t.Context(), f.binary, args...) //nolint:gosec // test-built executable
					command.Env = f.env
					output, err := command.CombinedOutput()
					require.NoError(t, err, "%s", output)
				}
				require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: the real hook reports it has started
					_, err := os.Stat(filepath.Join(f.home, "hook-ready"))
					return err == nil
				}, 5*time.Second, 20*time.Millisecond)
				restart := exec.CommandContext(t.Context(), f.cli, "daemon", "restart", "--json") //nolint:gosec // test-built executable
				restart.Env = f.env
				var restartOutput bytes.Buffer
				restart.Stdout, restart.Stderr = &restartOutput, &restartOutput
				require.NoError(t, restart.Start())
				restarted := make(chan struct{})
				var restartErr error
				go func() {
					restartErr = restart.Wait()
					close(restarted)
				}()
				t.Cleanup(func() {
					select {
					case <-restarted:
					default:
						_ = restart.Process.Kill()
						<-restarted
					}
				})
				require.Eventually(t, func() bool { return f.record().PID == 0 }, 5*time.Second, 20*time.Millisecond) //nolint:kennlint // sleeptest: graceful shutdown removes discovery before draining the real hook
				if scenario == "restart during drain" {
					require.NoError(t, restart.Process.Signal(os.Interrupt))
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					unlock, err := f.store.AcquireStartLock(ctx)
					if unlock != nil {
						unlock()
					}
					require.ErrorIs(t, err, context.DeadlineExceeded, "one Ctrl-C must retain handoff ownership during drain")
				}
				lockPath, err := f.store.LockPath()
				require.NoError(t, err)
				lockAttempted := watchOpens(t, lockPath)
				done := make(chan struct{}, 1)
				go func() {
					args := []string{"daemon", "restart", "--json"}
					if scenario == "start during drain" {
						args = []string{"daemon", "start", "--json"}
					}
					command := exec.CommandContext(t.Context(), f.cli, args...) //nolint:gosec // test-built executable
					command.Env = f.env
					output, err := command.CombinedOutput()
					assert.NoError(t, err, "%s", output)
					done <- struct{}{}
				}()
				lockAttempted() // the second CLI has opened the shared start lock
				select {
				case <-done:
					t.Fatal("second command finished before the restart drain was released")
				case <-time.After(200 * time.Millisecond): //nolint:kennlint // deadlinetest: a real CLI must wait for the held restart drain
				}
				require.NoError(t, os.WriteFile(release, nil, 0600))
				<-restarted
				assert.NoError(t, restartErr, "%s", restartOutput.String())
				<-done
				return
			}
			if scenario == "replacement fails after exec" {
				output, err := f.restart(t, "--json", "--listen=127.0.0.1:bad")
				require.Error(t, err)
				assert.Contains(t, parseErrorEnvelope(t, []byte(output)).Error.Message, "unknown port")
				assert.Zero(t, f.record().PID)
				return
			}
			if scenario == "older daemon" {
				delete(before.Metadata, daemonruntime.RuntimeRestartExecutableKey)
				_, err := f.store.Write(before)
				require.NoError(t, err)
			}
			if scenario == "restart failure" {
				require.NoError(t, os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("invalid = ["), 0600))
			}
			if scenario == "startup flags" {
				output, err := f.restart(t, "--listen=0.0.0.0:0", "--insecure-readonly", "--no-auto-token")
				require.NoError(t, err, "%s", output)
				after := f.record()
				require.True(t, after.StartedAt.After(before.StartedAt))
				replacement, err := process.NewProcess(int32(after.PID)) //nolint:gosec // PID belongs to the test-built daemon.
				require.NoError(t, err)
				args, err := replacement.CmdlineSliceWithContext(t.Context())
				require.NoError(t, err)
				for _, flag := range []string{"--listen=0.0.0.0:0", "--insecure-readonly=true", "--no-auto-token=true"} {
					assert.Contains(t, args, flag)
				}
				return
			}
			if scenario == "new config" {
				if runtime.GOOS != "windows" {
					require.NoError(t, os.Remove(f.binary))
				}
				require.NoError(t, os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("future_setting = 'accepted'\n"), 0600))
			}
			args := []string{"--json"}
			if scenario == "new config" {
				args = nil
			}
			stdout, updateErr := f.restart(t, args...)
			if scenario == "older daemon" {
				require.Error(t, updateErr)
				assert.Contains(t, stdout, "is from an older kata release")
				assert.Contains(t, stdout, "as a one-time step")
				assert.Contains(t, stdout, "run 'kata daemon stop', then 'kata daemon start' in its original host shell or service with its original startup options")
				assert.Contains(t, stdout, "(it was listening on "+before.Endpoint().Address+")")
				assert.NotContains(t, stdout, "--listen=")
				assert.Equal(t, before.StartedAt, f.record().StartedAt)
				httpClient, baseURL := client.LocalHTTPClient(before.Endpoint().ConfigAddress())
				status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/ping", nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusOK, status)
				return
			}
			if scenario == "restart failure" {
				require.Error(t, updateErr)
				message := parseErrorEnvelope(t, []byte(stdout)).Error.Message
				assert.Contains(t, message, "restart failed")
				assert.Contains(t, message, "parse "+filepath.Join(f.home, "config.toml"))
				assert.NotContains(t, message, "did not restart within")
				assert.Equal(t, before.StartedAt, f.record().StartedAt)
				httpClient, baseURL := client.LocalHTTPClient(before.Endpoint().ConfigAddress())
				status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/ping", nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusOK, status)
				require.NoError(t, os.Remove(filepath.Join(f.home, "config.toml")))
				output, err := f.restart(t, "--json")
				require.NoError(t, err, "%s", output)
				return
			}
			require.NoError(t, updateErr, "%s", stdout)
			after := f.record()
			require.True(t, after.StartedAt.After(before.StartedAt))
			assert.Equal(t, "v0.2.0", after.Version)
			assert.Equal(t, fmt.Sprintf("%d.%d", before.PID, before.StartedAt.UnixNano()), after.Metadata[daemonruntime.RuntimeRestartFromKey],
				"the replacement names the daemon it replaced so other waiters ignore it")
			if scenario == "new config" {
				assert.Contains(t, stdout, fmt.Sprintf("restarted pid=%d address=%s\n", after.PID, after.Endpoint().ConfigAddress()))
			}
			httpClient, baseURL := client.LocalHTTPClient(after.Endpoint().ConfigAddress())
			status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/ping", nil)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, status)
		})
	}
}

func TestWaitForDaemonReplacement(t *testing.T) {
	identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	t.Run("readiness after drain and startup", func(t *testing.T) {
		setupKataEnv(t)
		t.Setenv("KATA_DSN", "")
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		readyAfter := 40 * time.Second
		synctest.Test(t, func(t *testing.T) {
			previous := kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now()}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute+time.Second)
			defer cancel()
			published := make(chan error, 1)
			go func() {
				time.Sleep(readyAfter)
				_, err := store.Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now(), Address: "127.0.0.1:7777"})
				published <- err
			}()

			replacement, err := waitForDaemonReplacement(ctx, store, previous, "")

			require.NoError(t, <-published)
			require.NoError(t, err)
			assert.Equal(t, "127.0.0.1:7777", replacement.Address)
			assert.Equal(t, readyAfter, replacement.StartedAt.Sub(previous.StartedAt))
		})
	})
	t.Run("unrelated starter", func(t *testing.T) {
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		reply := filepath.Join(store.Dir, "restart-reply-caller")
		require.NoError(t, os.WriteFile(reply, nil, 0600))
		started := time.Now().UTC()
		identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
		require.True(t, ok)
		_, err := store.Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: started, Address: "127.0.0.1:7777"})
		require.NoError(t, err)
		previous := kitdaemon.RuntimeRecord{PID: 1 << 30, StartedAt: started.Add(-time.Second)}
		_, err = waitForDaemonReplacement(t.Context(), store, previous, reply)
		require.ErrorContains(t, err, "this restart was not applied")
	})
	t.Run("Unix replacement exits before readiness", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix replacement retains the previous PID")
		}
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		_, err := waitForDaemonReplacement(t.Context(), store, kitdaemon.RuntimeRecord{PID: 1 << 30, StartedAt: time.Now()}, "")
		assert.ErrorContains(t, err, "exited without starting a replacement")
	})
	t.Run("replacement process exits before readiness", func(t *testing.T) {
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		reply := filepath.Join(store.Dir, "restart-reply-caller")
		require.NoError(t, os.WriteFile(reply, nil, 0600))
		// A Windows daemon names the separate process it started before exiting.
		require.NoError(t, os.WriteFile(reply+".pid", []byte(strconv.Itoa(1<<30+4)), 0600))
		_, err := waitForDaemonReplacement(t.Context(), store, kitdaemon.RuntimeRecord{PID: 1 << 30, StartedAt: time.Now()}, reply)
		assert.ErrorContains(t, err, fmt.Sprintf("replacement pid %d exited before it was ready", 1<<30+4))
	})
	t.Run("replacement of another daemon", func(t *testing.T) {
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		reply := filepath.Join(store.Dir, "restart-reply-caller")
		require.NoError(t, os.WriteFile(reply, nil, 0600))
		synctest.Test(t, func(t *testing.T) {
			previous := kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now()}
			time.Sleep(time.Second)
			_, err := store.Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now(), Address: "127.0.0.1:7001", Metadata: map[string]string{
				daemonruntime.RuntimeRestartFromKey:  "4242.1700000000000000000",
				daemonruntime.RuntimeRestartReplyKey: "restart-reply-other",
			}})
			require.NoError(t, err)
			published := make(chan error, 1)
			go func() {
				time.Sleep(3 * time.Second)
				_, err := store.Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now(), Address: "127.0.0.1:7002", Metadata: map[string]string{
					daemonruntime.RuntimeRestartFromKey:  fmt.Sprintf("%d.%d", previous.PID, previous.StartedAt.UnixNano()),
					daemonruntime.RuntimeRestartReplyKey: filepath.Base(reply),
				}})
				published <- err
			}()

			replacement, err := waitForDaemonReplacement(t.Context(), store, previous, reply)

			require.NoError(t, <-published)
			require.NoError(t, err)
			assert.Equal(t, "127.0.0.1:7002", replacement.Address)
		})
	})
	t.Run("late readiness after previous exits", func(t *testing.T) {
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		reply := filepath.Join(store.Dir, "restart-reply-caller")
		require.NoError(t, os.WriteFile(reply, nil, 0600))
		// A Windows daemon names the separate process it started before exiting.
		require.NoError(t, os.WriteFile(reply+".pid", []byte(strconv.Itoa(os.Getpid())), 0600))
		identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
		require.True(t, ok)
		synctest.Test(t, func(t *testing.T) {
			previous := kitdaemon.RuntimeRecord{PID: 1 << 30, StartedAt: time.Now()}
			published := make(chan error, 1)
			go func() {
				time.Sleep(6 * time.Second)
				_, err := store.Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now(), Address: "127.0.0.1:7777", Metadata: map[string]string{daemonruntime.RuntimeRestartReplyKey: filepath.Base(reply)}})
				published <- err
			}()

			replacement, err := waitForDaemonReplacement(t.Context(), store, previous, reply)

			require.NoError(t, <-published)
			require.NoError(t, err)
			assert.Equal(t, os.Getpid(), replacement.PID)
		})
	})
}

func daemonLifecycleEnvironment(t *testing.T) []string {
	t.Helper()
	// Child daemons get only toolchain/OS variables and the test's Kata paths.
	var childEnv []string
	allowed := []string{"PATH", "HOME", "USERPROFILE", "LOCALAPPDATA", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT", "GOTOOLCHAIN", "CGO_CFLAGS", "CGO_LDFLAGS"}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "" {
			continue // Windows drive-directory entries cannot be changed with Setenv.
		}
		lookupKey := key
		if runtime.GOOS == "windows" {
			lookupKey = strings.ToUpper(key)
		}
		if slices.Contains(allowed, lookupKey) {
			childEnv = append(childEnv, entry)
		} else {
			t.Setenv(key, "")
		}
	}
	return childEnv
}

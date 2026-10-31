package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	childEnv := daemonLifecycleEnvironment(t)
	moduleDir, err := os.Getwd()
	require.NoError(t, err)
	workspace := t.TempDir()
	t.Chdir(workspace)
	binaries := make(map[string]string)
	for _, version := range []string{"v0.1.0", "v0.2.0"} {
		binary := filepath.Join(t.TempDir(), "kata")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-ldflags", "-X go.kenn.io/kata/internal/version.Version="+version, "-o", binary, "go.kenn.io/kata/cmd/kata") //nolint:gosec // test-owned executable and fixed build arguments
		// Build inside the module, while runtime commands use a scratch workspace.
		build.Dir = moduleDir
		if version == "v0.2.0" {
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
			build.Args = append(build.Args[:2], append([]string{"-overlay=" + overlayPath}, build.Args[2:]...)...)
		}
		build.Env = childEnv
		output, err := build.CombinedOutput()
		require.NoError(t, err, "%s", output)
		binaries[version] = binary
	}
	return func(t *testing.T, scenario string) *daemonLifecycleFixture {
		t.Helper()
		resetFlags(t)
		home, err := config.KataHome()
		require.NoError(t, err)
		binary := filepath.Join(t.TempDir(), filepath.Base(binaries["v0.1.0"]))
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
		if scenario == "daemon token" || strings.Contains(scenario, "during drain") {
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
			stop := exec.Command(binaries["v0.2.0"], "daemon", "stop") //nolint:gosec // test-owned binary
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
			case "daemon token", "restart during drain", "start during drain":
				args = append(args, "--listen", "127.0.0.1:0")
			}
			process := exec.Command(binary, args...) //nolint:gosec // test-owned binary and fixed arguments
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
}

func testUpdateInstallDaemonLifecycle(t *testing.T, fixture func(*testing.T, string) *daemonLifecycleFixture) {
	t.Helper()
	for _, scenario := range []string{"running", "stopped", "check only", "cancelled", "install failure", "read-only TCP", "older daemon", "daemon token", "idle shutdown"} {
		t.Run(scenario, func(t *testing.T) {
			fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{CurrentVersion: "v0.1.0", LatestVersion: "v0.2.0"}}}
			stubUpdateClient(t, fake)
			f := fixture(t, scenario)
			before := f.record()
			if scenario == "older daemon" {
				delete(before.Metadata, daemonruntime.RuntimeRestartMetadataKey)
				delete(before.Metadata, daemonruntime.RuntimeRestartExecutableKey)
				_, err := f.store.Write(before)
				require.NoError(t, err)
			}
			fake.install = func() error {
				if scenario == "install failure" {
					return errors.New("download failed")
				}
				if err := selfupdate.InstallBinary(f.cli, f.binary); err != nil {
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
				// Observe the actual exit without sending requests that reset the idle timer.
				require.Eventually(t, func() bool { return f.record().PID == 0 }, 20*time.Second, 50*time.Millisecond) //nolint:kennlint // sleeptest: idle shutdown is observed through the external daemon runtime record
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
	for _, scenario := range []string{"restart failure", "older daemon", "startup flags", "new config", "restart during drain", "start during drain"} {
		t.Run(scenario, func(t *testing.T) {
			if strings.Contains(scenario, "during drain") && runtime.GOOS != "linux" {
				t.Skip("holds drain with a shell hook and observes the start lock through inotify")
			}
			setupKataEnv(t)
			f := fixture(t, scenario)
			before := f.record()
			if scenario != "older daemon" && runtime.GOOS != "windows" {
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
				restart.Env = append(append([]string(nil), f.env...), "PORT=4321")
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
				lockAttempted() // the second CLI is retrying the start lock the first restart holds
				require.NoError(t, os.WriteFile(release, nil, 0600))
				<-restarted
				assert.NoError(t, restartErr, "%s", restartOutput.String())
				<-done
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
				assert.True(t, strings.HasPrefix(after.Address, "0.0.0.0:"), "%s", after.Address)
				assert.NotEqual(t, "0.0.0.0:0", after.Address)
				replacement, err := process.NewProcess(int32(after.PID)) //nolint:gosec // PID belongs to the test-built daemon.
				require.NoError(t, err)
				args, err := replacement.CmdlineSliceWithContext(t.Context())
				require.NoError(t, err)
				for _, flag := range []string{"--listen=0.0.0.0:0", "--insecure-readonly=true", "--no-auto-token=true"} {
					assert.Contains(t, args, flag)
				}
				output, err = f.restart(t, "--insecure-readonly=false")
				require.Error(t, err)
				assert.Contains(t, output, "pass --insecure-readonly")
				return
			}
			if scenario == "new config" {
				if runtime.GOOS != "windows" {
					require.NoError(t, os.Remove(f.binary))
				}
				require.NoError(t, os.WriteFile(filepath.Join(f.home, "config.toml"), []byte("future_setting = 'accepted'\n"), 0600))
			}
			stdout, updateErr := f.restart(t, "--json")
			if scenario == "older daemon" {
				require.Error(t, updateErr)
				assert.Contains(t, stdout+updateErr.Error(), "kata daemon stop")
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
				assert.Contains(t, message, "request refused")
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
				replies, err := filepath.Glob(filepath.Join(f.store.Dir, "restart*"))
				require.NoError(t, err)
				assert.Empty(t, replies)
				return
			}
			require.NoError(t, updateErr, "%s", stdout)
			after := f.record()
			require.True(t, after.StartedAt.After(before.StartedAt))
			assert.Equal(t, "v0.2.0", after.Version)
			httpClient, baseURL := client.LocalHTTPClient(after.Endpoint().ConfigAddress())
			status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/ping", nil)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, status)
		})
	}
}

func TestWaitForDaemonReplacement(t *testing.T) {
	t.Run("unrelated starter", func(t *testing.T) {
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		reply := filepath.Join(store.Dir, "restart-reply-caller")
		require.NoError(t, os.WriteFile(reply, nil, 0600))
		started := time.Now().UTC()
		_, err := store.Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), StartedAt: started, Address: "127.0.0.1:7777"})
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
		synctest.Test(t, func(t *testing.T) {
			started := time.Now()
			_, err := waitForDaemonReplacement(t.Context(), store, kitdaemon.RuntimeRecord{PID: 1 << 30, StartedAt: started}, "")
			assert.ErrorContains(t, err, "exited without starting a replacement")
			assert.Less(t, time.Since(started), time.Second)
		})
	})
	t.Run("late readiness after previous exits", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("Windows replacement starts a new PID")
		}
		store := kitdaemon.RuntimeStore{Dir: t.TempDir()}
		reply := filepath.Join(store.Dir, "restart-reply-caller")
		require.NoError(t, os.WriteFile(reply, nil, 0600))
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
			assert.True(t, replacement.StartedAt.After(previous.StartedAt))
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

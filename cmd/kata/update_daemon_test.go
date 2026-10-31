package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestUpdateInstall_DaemonLifecycle(t *testing.T) {
	testDaemonLifecycle(t, false)
}

func TestDaemonRestart_Lifecycle(t *testing.T) {
	testDaemonLifecycle(t, true)
}

func testDaemonLifecycle(t *testing.T, explicitRestart bool) {
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
		if explicitRestart && version == "v0.2.0" {
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
	scenarios := []string{"running", "stopped", "check only", "cancelled", "install failure", "restart failure", "read-only TCP", "older daemon", "daemon token", "idle shutdown"}
	if explicitRestart {
		scenarios = []string{"restart failure", "older daemon", "startup flags", "upgrade removed", "cancelled restart", "new config"}
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			resetFlags(t)
			fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{CurrentVersion: "v0.1.0", LatestVersion: "v0.2.0"}}}
			stubUpdateClient(t, fake)
			home, err := config.KataHome()
			require.NoError(t, err)
			binary := filepath.Join(t.TempDir(), filepath.Base(binaries["v0.1.0"]))
			// This is a fresh test path, so a direct copy is enough. Keep the
			// staged replacement exercised by fake.install below, where the
			// existing daemon binary really must be replaced.
			copyTestExecutable(t, binaries["v0.1.0"], binary)
			launchBinary := binary
			cli := binaries["v0.2.0"]
			if scenario == "upgrade removed" {
				if runtime.GOOS == "windows" {
					t.Skip("installation symlink regression is Unix-specific")
				}
				launchBinary = filepath.Join(t.TempDir(), "kata")
				require.NoError(t, os.Symlink(binary, launchBinary))
				cli = launchBinary
			}
			env := append(append([]string(nil), childEnv...), "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"), "KATA_WORKSPACE="+workspace, "KATA_AUTHOR=user-a")
			restartCLI := func(args ...string) (string, error) {
				command := exec.CommandContext(t.Context(), cli, append([]string{"daemon", "restart"}, args...)...) //nolint:gosec // G204: test-built executable
				command.Env = append(append([]string(nil), env...), "PORT=4321")
				var stderr bytes.Buffer
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					output = append(output, stderr.Bytes()...)
				}
				return string(output), err
			}
			// The daemon restarts from its own environment. A hosted PORT in the
			// updater's shell must not move a Unix-socket daemon onto wildcard TCP.
			t.Setenv("PORT", "4321")
			if scenario == "daemon token" {
				t.Setenv("KATA_AUTH_TOKEN", "caller-token")
				// Only the daemon holds the token; the updater never needs it.
				env = append(env, "KATA_AUTH_TOKEN=fixture-token")
			}
			if scenario == "idle shutdown" {
				env = append(env, "KATA_AUTOSTART=1", "KATA_AUTOSTART_IDLE_TIMEOUT=10s")
			}
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			record := func() kitdaemon.RuntimeRecord {
				records, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
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
				require.Eventually(t, func() bool { return record().PID == 0 }, 10*time.Second, 20*time.Millisecond)
			})
			if scenario != "stopped" {
				args := []string{"daemon", "start", "--foreground"}
				switch scenario {
				case "startup flags":
					args = append(args, "--")
				case "read-only TCP":
					args = append(args, "--listen", "127.0.0.1:0", "--insecure-readonly")
				case "daemon token":
					args = append(args, "--listen", "127.0.0.1:0")
				}
				process := exec.Command(launchBinary, args...) //nolint:gosec // test-owned binary and fixed arguments
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
				require.Eventually(t, func() bool { return record().PID != 0 }, 10*time.Second, 20*time.Millisecond)
			}
			before := record()
			if scenario == "older daemon" {
				// Older daemons can lack executable selection or the restart signal.
				if explicitRestart {
					delete(before.Metadata, daemonruntime.RuntimeRestartExecutableKey)
				} else {
					delete(before.Metadata, daemonruntime.RuntimeRestartMetadataKey)
				}
				_, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(before)
				require.NoError(t, err)
			}
			fake.install = func() error {
				if scenario == "install failure" {
					return errors.New("download failed")
				}
				if err := selfupdate.InstallBinary(binaries["v0.2.0"], binary); err != nil {
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
				assert.Equal(t, before.StartedAt, record().StartedAt)
				return
			}
			var stdout, stderr string
			var updateErr error
			if scenario == "restart failure" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("invalid = ["), 0600))
			}
			if explicitRestart {
				if scenario == "startup flags" {
					output, err := restartCLI("--listen=127.0.0.1:0", "--insecure-readonly", "--no-auto-token")
					require.NoError(t, err, "%s", output)
					after := record()
					require.True(t, after.StartedAt.After(before.StartedAt))
					assert.False(t, after.Endpoint().IsUnix())
					if runtime.GOOS != "windows" {
						assert.Equal(t, before.PID, after.PID)
					}
					return
				}
				if scenario == "upgrade removed" {
					require.NoError(t, os.Remove(launchBinary))
					require.NoError(t, os.Symlink(binaries["v0.2.0"], launchBinary))
					require.NoError(t, os.Remove(binary))
				}
				if scenario == "new config" {
					require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("future_setting = 'accepted'\n"), 0600))
				}
				if scenario == "cancelled restart" {
					if runtime.GOOS == "windows" {
						t.Skip("requires Unix process suspension")
					}
					signal := func(name string) {
						output, err := exec.Command("kill", name, fmt.Sprint(before.PID)).CombinedOutput() //nolint:gosec // G204: fixed suspension signals target the fixture PID
						require.NoError(t, err, "%s", output)
					}
					signal("-STOP")
					t.Cleanup(func() { signal("-CONT") })
					command := exec.CommandContext(t.Context(), cli, "daemon", "restart") //nolint:gosec // test-built executable
					command.Env = env
					require.NoError(t, command.Start())
					t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
					//nolint:kennlint // sleeptest: External CLI publishes its request through the filesystem.
					require.Eventually(t, func() bool { _, err := os.Stat(restartRequestPath(ns.DataDir, before)); return err == nil }, 10*time.Second, 20*time.Millisecond)
					require.NoError(t, command.Process.Signal(os.Interrupt))
					require.Error(t, command.Wait())
					replies, err := filepath.Glob(filepath.Join(ns.DataDir, "restart-reply-*"))
					require.NoError(t, err)
					assert.Empty(t, replies)
					signal("-CONT")
					//nolint:kennlint // sleeptest: External daemon publishes replacement readiness through its runtime record.
					require.Eventually(t, func() bool { return record().StartedAt.After(before.StartedAt) }, 10*time.Second, 20*time.Millisecond)
				} else {
					stdout, updateErr = restartCLI("--json")
				}
			} else {
				stdout, stderr, updateErr = executeRootCapture(t, t.Context(), "update", "--yes", "--json")
			}
			if scenario == "older daemon" {
				require.Error(t, updateErr)
				if !explicitRestart {
					assert.Contains(t, updateErr.Error(), "installed kata v0.2.0, but the daemon was not restarted")
					assert.NotEmpty(t, fake.installed)
				}
				assert.Contains(t, stdout+updateErr.Error(), "kata daemon stop")
				assert.Equal(t, before.StartedAt, record().StartedAt)
				httpClient, baseURL := client.LocalHTTPClient(before.Endpoint().ConfigAddress())
				status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/ping", nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusOK, status)
				return
			}
			if scenario == "restart failure" {
				require.Error(t, updateErr)
				assert.Contains(t, stdout+updateErr.Error(), "validate replacement")
				assert.Contains(t, stdout+updateErr.Error(), "parse "+filepath.Join(home, "config.toml"))
				assert.NotContains(t, stdout+updateErr.Error(), "did not restart within")
				assert.Equal(t, before.StartedAt, record().StartedAt)
				httpClient, baseURL := client.LocalHTTPClient(before.Endpoint().ConfigAddress())
				status, _, err := httpDoJSON(t.Context(), httpClient, http.MethodGet, baseURL+"/api/v1/ping", nil)
				require.NoError(t, err)
				assert.Equal(t, http.StatusOK, status)
				require.NoError(t, os.Remove(filepath.Join(home, "config.toml")))
				// A corrected request must still reach the signal watcher.
				_, err = restartCLI()
				require.NoError(t, err)
				require.True(t, record().StartedAt.After(before.StartedAt))
				return
			}
			if scenario == "install failure" {
				require.Error(t, updateErr)
				assert.Equal(t, before.StartedAt, record().StartedAt)
				return
			}
			require.NoError(t, updateErr, "%s", stderr)
			if explicitRestart {
				if scenario != "cancelled restart" {
					var restarted daemonRestartOutput
					require.NoError(t, json.Unmarshal([]byte(stdout), &restarted))
					assert.Equal(t, []int{before.PID}, restarted.PIDs)
				}
			} else {
				var result struct {
					Installed bool `json:"installed"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &result))
				assert.True(t, result.Installed)
			}
			after := record()
			if scenario == "stopped" {
				assert.Zero(t, after.PID)
				assert.NotContains(t, stderr, "restarted")
				return
			}
			require.True(t, after.StartedAt.After(before.StartedAt), "update must replace the running daemon")
			assert.Equal(t, "v0.2.0", after.Version)
			if !explicitRestart {
				assert.Contains(t, stderr, "restarted daemon")
			}
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
				require.Eventually(t, func() bool { return record().PID == 0 }, 20*time.Second, 50*time.Millisecond)
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

package main

import (
	"encoding/json"
	"errors"
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
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/selfupdate"
)

func TestUpdateInstall_DaemonLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs isolated daemon binaries")
	}
	// Child daemons get only toolchain/OS variables and the test's Kata paths.
	var childEnv []string
	allowed := []string{"PATH", "HOME", "USERPROFILE", "SystemRoot", "SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT", "GOTOOLCHAIN"}
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			childEnv = append(childEnv, key+"="+value)
		}
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(allowed, key) {
			t.Setenv(key, "")
		}
	}
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
		build := exec.CommandContext(t.Context(), "go", "build", "-tags", "kit_posthog_disabled", "-buildvcs=false", "-ldflags", "-X go.kenn.io/kata/internal/version.Version="+version, "-o", binary, "go.kenn.io/kata/cmd/kata") //nolint:gosec // test-owned executable and fixed build arguments
		// Build inside the module, while runtime commands use a scratch workspace.
		build.Dir = moduleDir
		build.Env = childEnv
		output, err := build.CombinedOutput()
		require.NoError(t, err, "%s", output)
		binaries[version] = binary
	}
	for _, scenario := range []string{"running", "stopped", "check only", "cancelled", "install failure", "restart failure", "read-only TCP"} {
		t.Run(scenario, func(t *testing.T) {
			resetFlags(t)
			fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{CurrentVersion: "v0.1.0", LatestVersion: "v0.2.0"}}}
			stubUpdateClient(t, fake)
			home, err := config.KataHome()
			require.NoError(t, err)
			binary := filepath.Join(t.TempDir(), filepath.Base(binaries["v0.1.0"]))
			require.NoError(t, selfupdate.InstallBinary(binaries["v0.1.0"], binary))
			originalExecutable := updateExecutable
			updateExecutable = func() (string, error) { return binary, nil }
			t.Cleanup(func() { updateExecutable = originalExecutable })
			env := append(append([]string(nil), childEnv...), "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"), "KATA_WORKSPACE="+workspace, "KATA_AUTHOR=user-a")
			ns, err := daemon.NewNamespace()
			require.NoError(t, err)
			record := func() kitdaemon.RuntimeRecord {
				records, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
				require.NoError(t, err)
				for _, rec := range records {
					if daemon.RuntimeProcessAlive(rec) {
						return rec
					}
				}
				return kitdaemon.RuntimeRecord{}
			}
			t.Cleanup(func() {
				stop := exec.Command(binary, "daemon", "stop") //nolint:gosec // test-owned binary
				stop.Env = env
				output, err := stop.CombinedOutput()
				assert.NoError(t, err, "%s", output)
				require.Eventually(t, func() bool { return record().PID == 0 }, 10*time.Second, 20*time.Millisecond)
			})
			if scenario != "stopped" {
				args := []string{"daemon", "start", "--foreground"}
				if scenario == "read-only TCP" {
					args = append(args, "--listen", "127.0.0.1:0", "--insecure-readonly")
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
				require.Eventually(t, func() bool { return record().PID != 0 }, 10*time.Second, 20*time.Millisecond)
			}
			before := record()
			fake.install = func() error {
				if scenario == "install failure" {
					return errors.New("download failed")
				}
				if err := selfupdate.InstallBinary(binaries["v0.2.0"], binary); err != nil {
					return err
				}
				if scenario == "restart failure" {
					return os.WriteFile(filepath.Join(home, "config.toml"), []byte("invalid = ["), 0600)
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
				assert.Equal(t, before.PID, record().PID)
				return
			}
			stdout, stderr, updateErr := executeRootCapture(t, t.Context(), "update", "--yes", "--json")
			if scenario == "restart failure" {
				require.Error(t, updateErr)
				assert.Contains(t, updateErr.Error(), "installed")
				assert.Contains(t, updateErr.Error(), "restart")
				// Restore the fixture config so cleanup can discover and stop the daemon.
				require.NoError(t, os.Remove(filepath.Join(home, "config.toml")))
				assert.Equal(t, before.PID, record().PID)
				return
			}
			if scenario == "install failure" {
				require.Error(t, updateErr)
				assert.Equal(t, before.PID, record().PID)
				return
			}
			require.NoError(t, updateErr, "%s", stderr)
			var result struct {
				Installed bool `json:"installed"`
			}
			require.NoError(t, json.Unmarshal([]byte(stdout), &result))
			assert.True(t, result.Installed)
			after := record()
			if scenario == "stopped" {
				assert.Zero(t, after.PID)
				return
			}
			require.NotEqual(t, before.PID, after.PID, "update must replace the running daemon")
			assert.Equal(t, "v0.2.0", after.Version)
			httpClient, baseURL := client.LocalHTTPClient(after.Endpoint().ConfigAddress())
			status, _, err := httpDoJSON(t.Context(), httpClient, "GET", baseURL+"/api/v1/ping", nil)
			require.NoError(t, err)
			assert.Equal(t, 200, status)
			if scenario == "read-only TCP" {
				assert.Equal(t, before.Address, after.Address)
				// Anonymous read-only mode must survive the automatic restart.
				status, _, err = httpDoJSON(t.Context(), httpClient, http.MethodPost, baseURL+"/api/v1/projects", map[string]string{"name": "example-project", "actor": "user-a"})
				require.NoError(t, err)
				assert.Equal(t, http.StatusUnauthorized, status)
			}
		})
	}
}

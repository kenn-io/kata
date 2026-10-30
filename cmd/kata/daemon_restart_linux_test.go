//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonRestart_PreservesNetworkNamespace(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs an isolated daemon")
	}
	childEnv := daemonLifecycleEnvironment(t)
	resetFlags(t)
	home := setupKataEnv(t)
	binary := filepath.Join(t.TempDir(), "kata")
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", binary, "go.kenn.io/kata/cmd/kata") //nolint:gosec // G204: fixed build arguments and a test-owned output path
	build.Env = childEnv
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	env := append(childEnv, "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"), "KATA_WORKSPACE="+t.TempDir())
	process := exec.Command(binary, "daemon", "start", "--foreground") //nolint:gosec // G204: test-built executable
	process.Env = env
	require.NoError(t, process.Start())
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()
	t.Cleanup(func() { _ = process.Process.Signal(syscall.SIGTERM); <-exited })
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	store := kitdaemon.RuntimeStore{Dir: ns.DataDir}
	var before kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: subprocess readiness is published through its runtime record
		records, err := store.List()
		if err == nil {
			for _, rec := range records {
				if daemonruntime.RuntimeProcessAlive(rec) {
					before = rec
					return true
				}
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	originalNetwork, err := os.Readlink("/proc/" + strconv.Itoa(before.PID) + "/ns/net")
	require.NoError(t, err)
	restart := exec.CommandContext(t.Context(), binary, "daemon", "restart", "--json") //nolint:gosec // G204: test-built executable
	restart.Env = env
	restart.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	output, err = restart.CombinedOutput()
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) {
		rootlesskit, lookupErr := exec.LookPath("rootlesskit")
		if lookupErr != nil {
			t.Skipf("user/network namespace unavailable: %v", err)
		}
		restart = exec.CommandContext(t.Context(), rootlesskit, "--net=slirp4netns", "--disable-host-loopback", binary, "daemon", "restart", "--json") //nolint:gosec // G204: installed namespace helper and test-built executable
		restart.Env = env
		// RootlessKit diagnostics go to stderr, leaving the CLI's JSON intact.
		restart.Stderr = os.Stderr
		output, err = restart.Output()
	}
	require.NoError(t, err, "%s", output)
	var result daemonRestartOutput
	require.NoError(t, json.Unmarshal(output, &result))
	// Clean up a caller-owned replacement too, so the baseline failure leaves no daemon.
	t.Cleanup(func() {
		if result.PID != process.Process.Pid {
			replacement, err := os.FindProcess(result.PID)
			if err == nil {
				_ = replacement.Signal(syscall.SIGTERM)
			}
		}
	})
	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.True(t, records[0].StartedAt.After(before.StartedAt))
	replacementNetwork, err := os.Readlink("/proc/" + strconv.Itoa(result.PID) + "/ns/net")
	require.NoError(t, err)
	require.Equal(t, originalNetwork, replacementNetwork, "restart must retain the host network")
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, result.WebURL, nil)
	require.NoError(t, err)
	request.Header.Set("Accept", "text/html")
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	require.NoError(t, err, "advertised browser URL must answer on the host")
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	t.Logf("network=%s replacement=%s browser=%s status=%d", originalNetwork, replacementNetwork, result.WebURL, response.StatusCode)
}

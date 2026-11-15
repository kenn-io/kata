//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/testenv"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestSocketActivationControlProcess(t *testing.T) {
	if os.Getenv("KATA_MANAGER_CONTROL_CHILD") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM)
	defer stop()
	fmt.Println("ready")
	<-ctx.Done()
}

func managerControlProcess(t *testing.T) (*os.Process, <-chan struct{}) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	// The test owns the process through cleanup, even after t.Context is canceled.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), exe, "-test.run=^TestSocketActivationControlProcess$") //nolint:gosec // G204: executes this test binary, never a supplied command.
	cmd.Env = append(os.Environ(), "KATA_MANAGER_CONTROL_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	return cmd.Process, done
}

func TestSocketActivationLiveMetadataPreventsIdleStop(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	process, _ := managerControlProcess(t)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	record := kitdaemon.RuntimeRecord{PID: process.Pid, Metadata: map[string]string{daemonruntime.RuntimeSocketActivationKey: "systemd"}}
	err = stopIdleShutdownDaemon(t.Context(), ns, record)
	requireActivationManagerRefusal(t, err, "systemd")
	require.NoError(t, process.Signal(syscall.Signal(0)), "manager-owned process must remain alive even without the config policy")
}

func TestSocketActivationLiveMetadataPreventsDetachedStart(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	process, _ := managerControlProcess(t)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: process.Pid, Network: "tcp", Address: strings.TrimPrefix(env.URL, "http://"), StartedAt: time.Now().UTC(),
		Metadata: map[string]string{daemonruntime.RuntimeSocketActivationKey: "systemd"},
	})
	require.NoError(t, err)
	_, err = defaultStartDetachedDaemon(t.Context(), "", false, true)
	requireActivationManagerRefusal(t, err, "systemd")
	require.NoError(t, process.Signal(syscall.Signal(0)))
}

func TestSocketActivationDeadMetadataDoesNotBlockNativeStop(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	process, done := managerControlProcess(t)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	store := kitdaemon.RuntimeStore{Dir: ns.DataDir}
	_, err = store.Write(kitdaemon.RuntimeRecord{
		PID: 1 << 30, Network: "unix", Address: "unix:///tmp/absent-manager.sock", StartedAt: time.Now().Add(-time.Hour),
		Metadata: map[string]string{daemonruntime.RuntimeSocketActivationKey: "systemd"},
	})
	require.NoError(t, err)
	_, err = store.Write(kitdaemon.RuntimeRecord{
		PID: process.Pid, Network: "unix", Address: "unix:///tmp/native-control.sock", StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "stop", "--json")
	require.NoError(t, err)
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

// Enabling activation in a home whose ordinary daemon is still running must
// leave a way to stop that daemon; otherwise its record blocks the service.
func TestSocketActivationConfigAllowsStoppingUnmanagedDaemon(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	process, done := managerControlProcess(t)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: process.Pid, Network: "unix", Address: "unix:///tmp/native-control.sock", StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \"systemd\"\n"), 0o600))
	stdout, _, err := executeRootCapture(t, t.Context(), "daemon", "stop")
	require.NoError(t, err)
	require.Contains(t, stdout, fmt.Sprintf("stopped pid=%d", process.Pid))
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

func TestSocketActivationLiveMetadataPreventsStop(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	process, _ := managerControlProcess(t)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: process.Pid, Network: "unix", Address: "unix:///tmp/managed-control.sock", StartedAt: time.Now().UTC(),
		Metadata: map[string]string{daemonruntime.RuntimeSocketActivationKey: "launchd:api"},
	})
	require.NoError(t, err)
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "stop")
	requireActivationManagerRefusal(t, err, "launchd:api")
	require.NoError(t, process.Signal(syscall.Signal(0)), "manager-owned daemon must keep running")
}

func TestSocketActivationUpdateRequiresManagerForLiveDaemon(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	process, _ := managerControlProcess(t)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: process.Pid, Network: "unix", Address: "unix:///tmp/managed-update.sock", StartedAt: time.Now().UTC(),
		Metadata: map[string]string{daemonruntime.RuntimeSocketActivationKey: "systemd", daemonruntime.RuntimeRestartMetadataKey: "1"},
	})
	require.NoError(t, err)
	err = restartDaemonAfterUpdate(t.Context(), new(bytes.Buffer), "", "v1.0.0")
	requireActivationManagerRefusal(t, err, "systemd")
	require.NoError(t, process.Signal(syscall.Signal(0)))
}

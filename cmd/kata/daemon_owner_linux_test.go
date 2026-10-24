package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/rootbridge"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonStartupForeignOwner(t *testing.T) {
	if os.Getenv("KATA_OWNER_HELPER") == "1" {
		ns, err := daemonruntime.NewNamespace()
		require.NoError(t, err)
		_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).AcquireOwnerLock(t.Context())
		require.NoError(t, err)
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		// Exit without releasing the lock to exercise kernel cleanup.
		return
	}
	resetFlags(t)
	home := setupKataEnv(t)
	child := exec.Command(os.Args[0], "-test.run=^TestDaemonStartupForeignOwner$") //nolint:gosec // test binary
	child.Env = append(os.Environ(), "KATA_OWNER_HELPER=1")
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) {
			t.Skipf("user/PID namespace unavailable: %v", err)
		}
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", ready)
	identity, ok := kitdaemon.ReadProcessIdentity(child.Process.Pid)
	require.True(t, ok)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	record := kitdaemon.RuntimeRecord{
		PID: 1, ProcessIdentityV2: identity, Network: "tcp", Address: "127.0.0.1:7777",
	}
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(record))
	path, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(record)
	require.NoError(t, err)
	before, err := os.ReadFile(path) //nolint:gosec // runtime record in the test's temporary directory
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	err = runDaemonProcess(ctx, "127.0.0.1:0", false, false, nil)
	cancel()
	require.ErrorContains(t, err, "daemon already running")
	assert.NoFileExists(t, filepath.Join(home, "kata.db"))
	after, err := os.ReadFile(path) //nolint:gosec // runtime record in the test's temporary directory
	require.NoError(t, err)
	assert.Equal(t, before, after)

	require.NoError(t, stdin.Close())
	require.NoError(t, child.Wait())
	waited = true
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	originalRun := runExternalRootRunner
	t.Cleanup(func() { runExternalRootRunner = originalRun })
	started := false
	runExternalRootRunner = func(context.Context, *rootbridge.Runner) error {
		started = true
		cancel()
		return nil
	}
	require.NoError(t, runDaemonProcess(ctx, "127.0.0.1:0", false, false, nil))
	assert.True(t, started, "a fresh sandbox starts over a dead sibling's record")
	assert.FileExists(t, filepath.Join(home, "kata.db"))
}

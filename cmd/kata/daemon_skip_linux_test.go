//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

// A record written in another PID namespace names its PID there, so stop must
// skip it and say why.
func TestDaemonStopNamesForeignNamespaceSkip(t *testing.T) {
	if os.Getenv("KATA_PIDNS_SKIP_HELPER") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		t.SkipNow()
	}
	resetFlags(t)
	setupKataEnv(t)
	child := exec.Command(os.Args[0], "-test.run=^TestDaemonStopNamesForeignNamespaceSkip$") //nolint:gosec // test binary
	child.Env = append(os.Environ(), "KATA_PIDNS_SKIP_HELPER=1")
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	if err := child.Start(); err != nil {
		_ = stdin.Close()
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS) {
			t.Skipf("user/PID namespace unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = child.Wait() })
	identity, ok := kitdaemon.ReadProcessIdentity(child.Process.Pid)
	require.True(t, ok)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: 1, ProcessIdentityV2: identity, StartedAt: time.Now().UTC(),
		Network: "tcp", Address: closedLoopbackAddr(t),
	})
	require.NoError(t, err)

	_, stderr, err := executeRootCapture(t, t.Context(), "--agent", "daemon", "stop")

	require.NoError(t, err)
	assert.Contains(t, stderr, "skipping daemon pid=1 (another PID namespace)\n")
}

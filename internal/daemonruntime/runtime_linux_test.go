//go:build linux

package daemonruntime_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestRuntimeProcessAliveRejectsForeignNamespace(t *testing.T) {
	if os.Getenv("KATA_PIDNS_HELPER") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		t.SkipNow()
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRuntimeProcessAliveRejectsForeignNamespace$") //nolint:gosec // test binary
	child.Env = append(os.Environ(), "KATA_PIDNS_HELPER=1")
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
	t.Cleanup(func() { _ = stdin.Close(); require.NoError(t, child.Wait()) })
	identity, ok := kitdaemon.ReadProcessIdentity(child.Process.Pid)
	require.True(t, ok)
	record := kitdaemon.RuntimeRecord{PID: 1, ProcessIdentityV2: identity}
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(record))

	assert.False(t, daemonruntime.RuntimeProcessAlive(record))
}

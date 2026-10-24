//go:build linux

package daemonruntime_test

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func foreignNamespaceRuntimeRecord(t *testing.T) kitdaemon.RuntimeRecord {
	t.Helper()
	if os.Getenv("KATA_PIDNS_HELPER") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		t.SkipNow()
	}
	child := exec.Command(os.Args[0], "-test.run=^TestRuntimeProcessAliveRequiresLocalIdentity$/^foreign$") //nolint:gosec // test binary
	child.Env = append(os.Environ(), "KATA_PIDNS_HELPER=1")
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	if err := child.Start(); err != nil {
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
	return record
}

//go:build linux

package client

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestLiveDaemonsUsingForeignNamespaceRecord(t *testing.T) {
	if os.Getenv("KATA_PIDNS_HELPER") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		t.SkipNow()
	}
	child := exec.Command(os.Args[0], "-test.run=^TestLiveDaemonsUsingForeignNamespaceRecord$") //nolint:gosec // test binary
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
	record := kitdaemon.RuntimeRecord{
		PID: 1, ProcessIdentityV2: identity, Network: "unix", Address: "/tmp/kata.sock",
	}
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(record))

	found := func(probeErr error) (n int) {
		for _, err := range liveDaemonsUsing(t.Context(), t.TempDir(), func(string) ([]kitdaemon.RuntimeRecord, error) {
			return []kitdaemon.RuntimeRecord{record}, nil
		}, func(context.Context, string) (string, PingInfo, error) {
			return "http://unix", PingInfo{PID: record.PID}, probeErr
		}) {
			require.NoError(t, err)
			n++
		}
		return n
	}
	assert.Equal(t, 1, found(nil), "a live daemon in another PID namespace is found by ping")
	assert.Equal(t, 0, found(errors.New("endpoint unavailable")), "an unreachable one is skipped, not reported as a local daemon")
}

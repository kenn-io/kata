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
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
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

	for _, tc := range []struct {
		name       string
		probeErr   error
		candidates int
	}{
		{"successful-probe", nil, 1},
		{"failed-probe", errors.New("endpoint unavailable"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			probeCalls := 0
			var candidates []liveDaemon
			for candidate, err := range liveDaemonsUsing(t.Context(), dir, func(gotDir string) ([]kitdaemon.RuntimeRecord, error) {
				require.Equal(t, dir, gotDir)
				return []kitdaemon.RuntimeRecord{record}, nil
			}, func(_ context.Context, address string) (string, PingInfo, error) {
				probeCalls++
				require.Equal(t, "unix:///tmp/kata.sock", address)
				return "http://unix", PingInfo{PID: record.PID}, tc.probeErr
			}) {
				require.NoError(t, err)
				candidates = append(candidates, candidate)
			}
			assert.Equal(t, 1, probeCalls, "discovery must probe foreign records")
			assert.Len(t, candidates, tc.candidates)
			if len(candidates) == 1 {
				assert.Equal(t, record, candidates[0].Record)
				assert.Equal(t, record.PID, candidates[0].Info.PID)
			}
		})
	}
}

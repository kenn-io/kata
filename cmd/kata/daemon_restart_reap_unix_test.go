//go:build !windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRestartHelperIsReaped(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0") //nolint:noctx // the reaper owns this child
	require.NoError(t, cmd.Start())
	previous := startedForRestartHelper
	startedForRestartHelper = strconv.Itoa(cmd.Process.Pid)
	t.Cleanup(func() { startedForRestartHelper = previous })
	reapRestartHelper()
	require.Eventually(t, func() bool { return syscall.Kill(cmd.Process.Pid, 0) == syscall.ESRCH }, 5*time.Second, 10*time.Millisecond) //nolint:kennlint // sleeptest: observes the child being reaped
	_ = cmd.Process.Release()
}

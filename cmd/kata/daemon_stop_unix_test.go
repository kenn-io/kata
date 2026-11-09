//go:build !windows

package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonStop_UnreapedChild(t *testing.T) {
	resetFlags(t)
	home := setupKataEnv(t)
	// t.Cleanup owns this process; t.Context() would kill it before that runs.
	child := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=TestDaemonCommandSleepHelperProcess", "--") //nolint:gosec // test helper starts this test binary
	child.Env = append(os.Environ(), "KATA_DAEMON_CMD_SLEEP_HELPER=1", "KATA_DAEMON_CMD_REMOVE_RECORD=1")
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = stdin.Close(); _ = child.Process.Kill(); _ = child.Wait() })
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	writeRuntimePID(t, home, child.Process.Pid)
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	recordPath, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Path(child.Process.Pid)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err = executeRootCapture(t, ctx, "daemon", "stop")
	require.NoError(t, err)
	require.NoFileExists(t, recordPath, "stop must wait for the child to remove its runtime record")
}

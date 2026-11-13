//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"

	kitdaemon "go.kenn.io/kit/daemon"
)

var _ = takeEnv(daemonRestartHelperEnv)

func reapRestartHelper() {}

// reexecDaemon starts a detached replacement with this process's arguments,
// environment, and standard streams. Windows cannot replace a process image,
// so started receives the replacement's PID before this process exits.
func reexecDaemon(ctx context.Context, executable string, args, env []string, started func(pid int)) error {
	return kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable: executable,
		Args:       args,
		Env:        env,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		AfterStart: func(cmd *exec.Cmd) { started(cmd.Process.Pid) },
	})
}

//go:build windows

package main

import (
	"context"
	"os"

	kitdaemon "go.kenn.io/kit/daemon"
)

// reexecDaemon starts a detached replacement with this process's arguments,
// environment, and standard streams. Windows cannot replace a process image.
func reexecDaemon(ctx context.Context, executable string, args, env []string) error {
	return kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable: executable,
		Args:       args,
		Env:        env,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	})
}

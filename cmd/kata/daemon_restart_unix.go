//go:build !windows

package main

import (
	"context"
	"os"
	"syscall"
)

// reexecDaemon replaces the current process image. It only returns on failure.
func reexecDaemon(_ context.Context, executable string, args []string) error {
	return syscall.Exec(executable, append([]string{os.Args[0]}, args...), os.Environ()) //nolint:gosec // the daemon re-executes its own resolved binary with its own arguments
}

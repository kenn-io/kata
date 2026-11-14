//go:build !windows

package main

import (
	"context"
	"os"
	"strconv"
	"syscall"
)

var startedForRestartHelper = takeEnv(daemonRestartHelperEnv)

func reapRestartHelper() {
	pid, err := strconv.Atoi(startedForRestartHelper)
	if err != nil || pid <= 0 {
		return
	}
	go func() { _, _ = syscall.Wait4(pid, nil, 0, nil) }()
}

// reexecDaemon replaces the current process image, keeping its PID, so it
// never reports a separate replacement process. It only returns on failure.
func reexecDaemon(_ context.Context, executable string, args, env []string, _ func(pid int)) error {
	return syscall.Exec(executable, append([]string{os.Args[0]}, args...), env) //nolint:gosec // the daemon re-executes its own resolved binary with its own arguments
}

//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// spawnOrphanAndExit forks a child that ignores SIGTERM and sleeps,
// then keeps the parent alive ignoring SIGTERM for the same duration.
// Both parent and child must be killed via the process-group SIGKILL
// for the test to pass — the runner can't fall back to "process exited
// normally" because neither the parent nor the child terminates on
// SIGTERM. Used by the runner test suite to verify Setpgid + group
// SIGKILL escalation.
func spawnOrphanAndExit(d time.Duration) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Re-exec ourselves; exe path is from os.Executable, not user input.
	cmd := exec.Command(exe, "term-ignore", d.String()) //nolint:gosec,noctx // self-exec for orphan-spawn test helper; the child must outlive this process
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: false}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	signal.Ignore(syscall.SIGTERM)
	time.Sleep(d)
}

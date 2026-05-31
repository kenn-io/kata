//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/kata/internal/daemon"
)

// installStopWatcher is a no-op on Unix. The daemon's parent context is
// already wrapped with signal.NotifyContext(SIGINT, SIGTERM) in main.go,
// so external `kata daemon stop` (which sends SIGTERM) drives shutdown
// without any extra plumbing.
func installStopWatcher(_ string, _ context.CancelFunc) func() { return func() {} }

// installReloadSource hooks the existing SIGHUP delivery onto a channel
// the reload loop reads from. Cleanup detaches the signal handler.
func installReloadSource(_ context.Context, _ string) (<-chan os.Signal, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP)
	return sigs, func() { signal.Stop(sigs) }
}

// signalDaemonStop sends SIGTERM to the daemon process so its
// signal.NotifyContext-derived ctx cancels and the deferred cleanup runs.
func signalDaemonStop(rec daemon.RuntimeRecord, _ string) error {
	p, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", rec.PID, err)
	}
	return p.Signal(syscall.SIGTERM)
}

// signalDaemonReload sends SIGHUP to the daemon, picked up by runReloadLoop.
func signalDaemonReload(rec daemon.RuntimeRecord, _ string) error {
	p, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", rec.PID, err)
	}
	return p.Signal(syscall.SIGHUP)
}

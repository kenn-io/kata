//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
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

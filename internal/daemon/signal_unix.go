//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"syscall"

	kitdaemon "go.kenn.io/kit/daemon"
)

// SignalDaemonStop asks a running daemon to shut down gracefully.
func SignalDaemonStop(rec kitdaemon.RuntimeRecord, _ string) error {
	p, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", rec.PID, err)
	}
	return p.Signal(syscall.SIGTERM)
}

// SignalDaemonReload asks a running daemon to reload hook configuration.
func SignalDaemonReload(rec kitdaemon.RuntimeRecord, _ string) error {
	p, err := os.FindProcess(rec.PID)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", rec.PID, err)
	}
	return p.Signal(syscall.SIGHUP)
}

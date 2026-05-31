//go:build !windows

package main

import "testing"

// registerDaemonSignalEndpoints is a no-op on Unix, where `kata daemon stop`
// and `reload` deliver SIGTERM/SIGHUP directly to the PID rather than through
// the per-daemon named events used on Windows.
func registerDaemonSignalEndpoints(_ *testing.T, _ string, _ int) {}

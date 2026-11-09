//go:build windows

package main

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"

	"go.kenn.io/kata/internal/daemonruntime"
)

// registerDaemonSignalEndpoints creates the manual-reset named events that a
// real daemon stands up at startup (installStopWatcher / installReloadSource /
// installRestartSource),
// so `kata daemon stop`/`reload` can OpenEvent them against a faked daemon PID
// in tests. The handles are held open until test cleanup, keeping the events
// alive for the in-process command's OpenEvent call. A helper child may already
// own the stop event. On Unix this is a no-op.
func registerDaemonSignalEndpoints(t *testing.T, dbhash string, pid int) {
	t.Helper()
	for _, name := range []string{daemonruntime.StopEventName(dbhash, pid), daemonruntime.ReloadEventName(dbhash, pid), daemonruntime.RestartEventName(dbhash, pid)} {
		namePtr, err := windows.UTF16PtrFromString(name)
		if err != nil {
			t.Fatalf("event name %q: %v", name, err)
		}
		h, err := windows.CreateEvent(nil, 1, 0, namePtr) // manual reset, unsignaled
		if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			t.Fatalf("create event %s: %v", name, err)
		}
		t.Cleanup(func() { _ = windows.CloseHandle(h) })
	}
}

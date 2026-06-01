//go:build windows

package main

import (
	"testing"

	"golang.org/x/sys/windows"

	"go.kenn.io/kata/internal/daemon"
)

// registerDaemonSignalEndpoints creates the manual-reset named events that a
// real daemon stands up at startup (installStopWatcher / installReloadSource),
// so `kata daemon stop`/`reload` can OpenEvent them against a faked daemon PID
// in tests. The handles are held open until test cleanup, keeping the events
// alive for the in-process command's OpenEvent call. On Unix this is a no-op.
func registerDaemonSignalEndpoints(t *testing.T, dbhash string, pid int) {
	t.Helper()
	for _, name := range []string{daemon.StopEventName(dbhash, pid), daemon.ReloadEventName(dbhash, pid)} {
		namePtr, err := windows.UTF16PtrFromString(name)
		if err != nil {
			t.Fatalf("event name %q: %v", name, err)
		}
		h, err := windows.CreateEvent(nil, 1, 0, namePtr) // manual reset, unsignaled
		if err != nil {
			t.Fatalf("create event %s: %v", name, err)
		}
		t.Cleanup(func() { _ = windows.CloseHandle(h) })
	}
}

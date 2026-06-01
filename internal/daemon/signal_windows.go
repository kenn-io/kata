//go:build windows

package daemon

import (
	"fmt"

	kitdaemon "go.kenn.io/kit/daemon"
	"golang.org/x/sys/windows"
)

// StopEventName is the named-event endpoint a Windows daemon creates so
// another kata process can request graceful shutdown.
func StopEventName(dbhash string, pid int) string {
	return fmt.Sprintf(`Local\kata-stop-%s-%d`, dbhash, pid)
}

// ReloadEventName is the named-event endpoint a Windows daemon creates so
// another kata process can request hook reload.
func ReloadEventName(dbhash string, pid int) string {
	return fmt.Sprintf(`Local\kata-reload-%s-%d`, dbhash, pid)
}

// SignalDaemonStop asks a running daemon to shut down gracefully.
func SignalDaemonStop(rec kitdaemon.RuntimeRecord, dbhash string) error {
	return setNamedEvent(StopEventName(dbhash, rec.PID))
}

// SignalDaemonReload asks a running daemon to reload hook configuration.
func SignalDaemonReload(rec kitdaemon.RuntimeRecord, dbhash string) error {
	return setNamedEvent(ReloadEventName(dbhash, rec.PID))
}

func setNamedEvent(name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("event name %q: %w", name, err)
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, namePtr)
	if err != nil {
		return fmt.Errorf("open event %s: %w", name, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // close is best-effort
	if err := windows.SetEvent(h); err != nil {
		return fmt.Errorf("set event %s: %w", name, err)
	}
	return nil
}

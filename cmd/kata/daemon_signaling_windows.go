//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"

	"go.kenn.io/kata/internal/daemon"
)

// Windows has no SIGTERM/SIGHUP that can be sent across processes the way
// Unix uses them. We instead expose two manual-reset named events per
// running daemon:
//
//	Local\kata-stop-<dbhash>-<pid>     — set by `kata daemon stop`
//	Local\kata-reload-<dbhash>-<pid>   — set by `kata daemon reload`
//
// The daemon creates the events at startup, waits on them in goroutines,
// and translates a fire into ctx cancel (stop) or a synthetic SIGHUP fed
// to the existing reload loop.

func stopEventName(dbhash string, pid int) string {
	return fmt.Sprintf(`Local\kata-stop-%s-%d`, dbhash, pid)
}

func reloadEventName(dbhash string, pid int) string {
	return fmt.Sprintf(`Local\kata-reload-%s-%d`, dbhash, pid)
}

// installStopWatcher creates the stop event for this daemon process and
// spawns a goroutine that waits on it. When the event fires (and we are
// not already cleaning up) it cancels the daemon's context.
func installStopWatcher(dbhash string, cancel context.CancelFunc) func() {
	namePtr, err := windows.UTF16PtrFromString(stopEventName(dbhash, os.Getpid()))
	if err != nil {
		return func() {}
	}
	h, err := windows.CreateEvent(nil, 1, 0, namePtr) // manual reset, not signaled
	if err != nil {
		return func() {}
	}
	closing := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
		select {
		case <-closing:
			return
		default:
			cancel()
		}
	}()
	return func() {
		close(closing)
		_ = windows.SetEvent(h) // unblock the waiter so it can observe `closing`
		<-done
		_ = windows.CloseHandle(h)
	}
}

// installReloadSource creates the reload event and pumps a synthetic
// syscall.SIGHUP onto the returned channel each time it fires, so the
// existing runReloadLoop machinery works unchanged.
func installReloadSource(ctx context.Context, dbhash string) (<-chan os.Signal, func()) {
	sigs := make(chan os.Signal, 1)
	namePtr, err := windows.UTF16PtrFromString(reloadEventName(dbhash, os.Getpid()))
	if err != nil {
		return sigs, func() {}
	}
	h, err := windows.CreateEvent(nil, 1, 0, namePtr)
	if err != nil {
		return sigs, func() {}
	}
	closing := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, werr := windows.WaitForSingleObject(h, windows.INFINITE)
			select {
			case <-closing:
				return
			case <-ctx.Done():
				return
			default:
			}
			if werr != nil {
				return
			}
			_ = windows.ResetEvent(h)
			select {
			case sigs <- syscall.SIGHUP:
			default:
			}
		}
	}()
	return sigs, func() {
		close(closing)
		_ = windows.SetEvent(h)
		<-done
		_ = windows.CloseHandle(h)
	}
}

// signalDaemonStop opens the daemon's stop event by name and sets it.
// The daemon's installStopWatcher goroutine then drives a graceful exit.
func signalDaemonStop(rec daemon.RuntimeRecord, dbhash string) error {
	return setNamedEvent(stopEventName(dbhash, rec.PID))
}

// signalDaemonReload opens the daemon's reload event by name and sets it.
func signalDaemonReload(rec daemon.RuntimeRecord, dbhash string) error {
	return setNamedEvent(reloadEventName(dbhash, rec.PID))
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

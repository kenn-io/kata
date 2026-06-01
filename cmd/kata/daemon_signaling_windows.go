//go:build windows

package main

import (
	"context"
	"os"

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
// and translates a fire into ctx cancel (stop) or a synthetic reload fed
// to the existing reload loop.

// installStopWatcher creates the stop event for this daemon process and
// spawns a goroutine that waits on it. When the event fires (and we are
// not already cleaning up) it cancels the daemon's context.
func installStopWatcher(dbhash string, cancel context.CancelFunc) func() {
	namePtr, err := windows.UTF16PtrFromString(daemon.StopEventName(dbhash, os.Getpid()))
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

type syntheticReloadSignal struct{}

func (syntheticReloadSignal) String() string { return "reload" }
func (syntheticReloadSignal) Signal()        {}

// installReloadSource creates the reload event and pumps a private synthetic
// signal onto the returned channel each time it fires, so the existing
// runReloadLoop machinery works unchanged.
func installReloadSource(ctx context.Context, dbhash string) (<-chan os.Signal, func()) {
	sigs := make(chan os.Signal, 1)
	namePtr, err := windows.UTF16PtrFromString(daemon.ReloadEventName(dbhash, os.Getpid()))
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
			case sigs <- syntheticReloadSignal{}:
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

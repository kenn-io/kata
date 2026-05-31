//go:build windows

package daemon

import (
	"golang.org/x/sys/windows"
)

// stillActive is the value GetExitCodeProcess returns for a running
// process. Defined in winbase.h as STILL_ACTIVE (= STATUS_PENDING).
const stillActive uint32 = 259

// ProcessAlive returns true when pid identifies a process whose
// exit code is STILL_ACTIVE. PROCESS_QUERY_LIMITED_INFORMATION is the
// least-privileged right that can read the exit code; it works for any
// process the current user can normally see, including ones started by
// the same user from a different shell.
//
// A process that genuinely exits with code 259 will appear "alive" to
// this check; we accept that ambiguity because callers only ever feed
// in PIDs harvested from kata's own runtime files.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // close is best-effort; nothing to do on error
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// ProcessAlive returns true if a kill(0, pid) succeeds.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

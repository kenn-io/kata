// Package processtree manages platform-appropriate subprocess cleanup.
package processtree

import (
	"errors"
	"os/exec"
	"time"
)

// Prepare configures cmd for process grouping where the platform supports it.
func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
}

// TerminateWithGrace requests graceful termination, then escalates after grace
// for the process scope supported by the platform.
func TerminateWithGrace(cmd *exec.Cmd, grace time.Duration) error {
	if cmd.Process == nil {
		return nil
	}
	var errs []error
	if err := terminate(cmd); err != nil {
		errs = append(errs, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && alive(cmd) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(cmd) {
		if err := kill(cmd); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

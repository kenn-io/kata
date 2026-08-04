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

// TerminateWithGrace sends a graceful signal to cmd's process group on Unix,
// then force-kills the group if it survives grace. On Windows, it waits for
// grace and then force-kills only cmd's process if it is still running.
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

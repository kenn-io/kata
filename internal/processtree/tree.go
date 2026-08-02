// Package processtree manages subprocess groups during cancellation and cleanup.
package processtree

import (
	"errors"
	"os/exec"
	"time"
)

// Prepare configures cmd so its process tree can be terminated as a unit.
func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
}

// TerminateWithGrace stops cmd's process tree, escalating after grace expires.
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

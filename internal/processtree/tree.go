package processtree

import (
	"errors"
	"os/exec"
	"time"
)

func Prepare(cmd *exec.Cmd) {
	prepare(cmd)
}

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

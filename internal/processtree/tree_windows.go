//go:build windows

package processtree

import (
	"errors"
	"os"
	"os/exec"
)

func prepare(_ *exec.Cmd) {}

func terminate(_ *exec.Cmd) error { return nil }

func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func alive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
}

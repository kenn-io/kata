//go:build windows

package processtree

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

const stillActiveExitCode = 259

func prepare(_ *exec.Cmd) {}

func terminate(_ *exec.Cmd) error { return nil }

func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return killResult(err, false, nil)
	}
	exited, statusErr := processExited(cmd)
	return killResult(err, exited, statusErr)
}

func killResult(err error, exited bool, statusErr error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) &&
		((statusErr == nil && exited) || errors.Is(statusErr, os.ErrProcessDone)) {
		return nil
	}
	return err
}

func processExited(cmd *exec.Cmd) (bool, error) {
	var exitCode uint32
	var statusErr error
	if err := cmd.Process.WithHandle(func(handle uintptr) {
		statusErr = windows.GetExitCodeProcess(windows.Handle(handle), &exitCode)
	}); err != nil {
		return false, err
	}
	if statusErr != nil {
		return false, statusErr
	}
	return exitCode != stillActiveExitCode, nil
}

func alive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
}

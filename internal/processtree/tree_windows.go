//go:build windows

package processtree

import "os/exec"

func prepare(_ *exec.Cmd) {}

func terminate(_ *exec.Cmd) error { return nil }

func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func alive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
}

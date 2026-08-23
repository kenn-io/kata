//go:build !windows

package processtree

import (
	"errors"
	"os/exec"
	"syscall"
)

type platformTree struct {
	pgid int
}

func newPlatformTree(cmd *exec.Cmd) (platformTree, error) {
	prepare(cmd)
	return platformTree{}, nil
}

func (t *platformTree) start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	t.pgid = cmd.Process.Pid
	return nil
}

func (t *platformTree) close() error {
	if t.pgid == 0 {
		return nil
	}
	pgid := t.pgid
	t.pgid = 0
	return signal(-pgid, syscall.SIGKILL)
}

func (platformTree) terminate(cmd *exec.Cmd) error {
	return terminate(cmd)
}

func (platformTree) kill(cmd *exec.Cmd) error {
	return kill(cmd)
}

func (platformTree) alive(cmd *exec.Cmd) bool {
	return alive(cmd)
}

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return signal(-cmd.Process.Pid, syscall.SIGTERM)
}

func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return signal(-cmd.Process.Pid, syscall.SIGKILL)
}

func signal(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func alive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	err := syscall.Kill(-cmd.Process.Pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

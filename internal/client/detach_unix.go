//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// detachChild puts the spawned process in its own process group so signals
// delivered to the parent's process group (e.g. SIGINT from a tty ctrl-C)
// do not also kill the auto-started daemon.
func detachChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

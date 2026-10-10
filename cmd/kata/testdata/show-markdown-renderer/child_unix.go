//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureShowMarkdownHelperChild(child *exec.Cmd) {
	if os.Getenv("GO_WANT_SHOW_MARKDOWN_HELPER_DETACH") == "1" {
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

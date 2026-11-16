//go:build linux || darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// Replace fd 2 itself so direct writes, runtime diagnostics, and exec inherit it.
func redirectDaemonStderr(file *os.File) error {
	return unix.Dup2(int(file.Fd()), int(os.Stderr.Fd()))
}

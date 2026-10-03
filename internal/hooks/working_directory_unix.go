//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package hooks

import (
	"os"

	"golang.org/x/sys/unix"
)

func workingDirectoryAvailable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS) == nil
}

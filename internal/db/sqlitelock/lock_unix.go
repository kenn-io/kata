//go:build !windows

package sqlitelock

import (
	"os"

	"golang.org/x/sys/unix"
)

func tryDatabaseLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

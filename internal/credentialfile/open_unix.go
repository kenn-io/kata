//go:build unix

// Package credentialfile opens operator-configured credential files.
package credentialfile

import (
	"os"

	"golang.org/x/sys/unix"
)

// Open opens a credential file without blocking on a FIFO.
func Open(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

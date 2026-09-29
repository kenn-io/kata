//go:build unix

package config

import (
	"os"

	"golang.org/x/sys/unix"
)

func openEmbeddingCredentialFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var fileInfo unix.Stat_t
	if err := unix.Fstat(fd, &fileInfo); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if int64(fileInfo.Uid) != int64(os.Geteuid()) {
		_ = unix.Close(fd)
		return nil, errEmbeddingCredentialWrongOwner
	}
	return os.NewFile(uintptr(fd), path), nil
}

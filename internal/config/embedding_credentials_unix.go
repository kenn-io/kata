//go:build unix

package config

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openEmbeddingCredentialFile(path string) (*os.File, error) {
	dirName := "."
	components := strings.Split(path, string(os.PathSeparator))
	if filepath.IsAbs(path) {
		dirName = string(os.PathSeparator)
		components = strings.Split(strings.TrimLeft(path, string(os.PathSeparator)), string(os.PathSeparator))
	}
	if len(components) == 0 || components[len(components)-1] == "" ||
		components[len(components)-1] == "." || components[len(components)-1] == ".." {
		return nil, os.ErrInvalid
	}

	dirFD, err := unix.Open(dirName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirFD) }()
	var currentDirectory unix.Stat_t
	if err := unix.Fstat(dirFD, &currentDirectory); err != nil {
		return nil, err
	}
	if !embeddingCredentialDirectoryTrusted(&currentDirectory) {
		return nil, errEmbeddingCredentialUnsafeDir
	}
	for _, component := range components[:len(components)-1] {
		switch component {
		case "", ".":
			continue
		case "..":
			nextFD, err := openEmbeddingCredentialDirectory(dirFD, component)
			if err != nil {
				return nil, err
			}
			_ = unix.Close(dirFD)
			dirFD = nextFD
		default:
			var entry unix.Stat_t
			if err := unix.Fstatat(dirFD, component, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return nil, err
			}
			if entry.Mode&unix.S_IFMT == unix.S_IFLNK {
				return nil, errEmbeddingCredentialSymlink
			}
			if entry.Mode&unix.S_IFMT != unix.S_IFDIR {
				return nil, unix.ENOTDIR
			}
			nextFD, err := openEmbeddingCredentialDirectory(dirFD, component)
			if err != nil {
				return nil, err
			}
			_ = unix.Close(dirFD)
			dirFD = nextFD
		}
	}

	name := components[len(components)-1]
	var entry unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if entry.Mode&unix.S_IFMT == unix.S_IFLNK {
		return nil, errEmbeddingCredentialSymlink
	}
	fd, err := unix.Openat(dirFD, name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
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

func openEmbeddingCredentialDirectory(dirFD int, name string) (int, error) {
	nextFD, err := unix.Openat(dirFD, name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var directory unix.Stat_t
	if err := unix.Fstat(nextFD, &directory); err != nil {
		_ = unix.Close(nextFD)
		return -1, err
	}
	if !embeddingCredentialDirectoryTrusted(&directory) {
		_ = unix.Close(nextFD)
		return -1, errEmbeddingCredentialUnsafeDir
	}
	return nextFD, nil
}

func embeddingCredentialDirectoryTrusted(info *unix.Stat_t) bool {
	uid := int64(os.Geteuid())
	if int64(info.Uid) != uid && info.Uid != 0 {
		return false
	}
	return info.Mode&0o022 == 0 || info.Mode&0o1000 != 0
}

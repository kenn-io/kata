//go:build !windows

package main

import (
	"io/fs"
	"syscall"
)

// singleLinkFile reports whether info describes a file with one hard link.
func singleLinkFile(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

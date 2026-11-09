//go:build linux

package main

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// watchOpens observes Kit reopening its start lock on each blocked retry.
func watchOpens(t *testing.T, path string) func() {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	require.NoError(t, err)
	events := os.NewFile(uintptr(fd), "inotify")
	t.Cleanup(func() { _ = events.Close() })
	_, err = unix.InotifyAddWatch(fd, path, unix.IN_OPEN)
	require.NoError(t, err)
	return func() {
		t.Helper()
		require.NoError(t, events.SetReadDeadline(time.Now().Add(10*time.Second)))
		_, err := events.Read(make([]byte, 4096))
		require.NoError(t, err, "no caller tried the start lock at %s", path)
	}
}

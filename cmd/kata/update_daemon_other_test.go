//go:build !linux

package main

import "testing"

func watchOpens(t *testing.T, _ string) func() {
	t.Helper()
	t.Skip("needs Linux inotify")
	return nil
}

//go:build windows

package client

import "os/exec"

// detachChild is a no-op on Windows; the kata daemon does not officially
// support Windows in Plan 1 (Unix sockets only), but the build must still
// compile for go test ./... on Windows CI.
func detachChild(_ *exec.Cmd) {}

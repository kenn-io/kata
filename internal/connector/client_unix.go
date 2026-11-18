//go:build !windows

package connector

import (
	"errors"
	"syscall"
)

// brokenPipe reports whether a write failed because the reader closed its end.
func brokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

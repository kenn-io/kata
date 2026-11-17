//go:build windows

package connector

import (
	"errors"

	"golang.org/x/sys/windows"
)

// brokenPipe reports whether a write failed because the reader closed its end.
func brokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}

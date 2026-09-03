//go:build windows

package main

import (
	"errors"
	"syscall"
)

func connDroppedErrno(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) ||
		errors.Is(err, syscall.WSAECONNABORTED) ||
		errors.Is(err, syscall.ERROR_BROKEN_PIPE)
}

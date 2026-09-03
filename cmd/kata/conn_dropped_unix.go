//go:build !windows

package main

import (
	"errors"
	"syscall"
)

func connDroppedErrno(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

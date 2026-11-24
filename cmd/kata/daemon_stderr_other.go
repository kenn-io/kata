//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

func redirectDaemonStderr(_ *os.File) error {
	return errors.New("stderr redirection unsupported")
}

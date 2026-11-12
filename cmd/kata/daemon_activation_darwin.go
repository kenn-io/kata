//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/bored-engineer/go-launchd"

	"golang.org/x/sys/unix"
)

func activationFiles(ctx context.Context, mode string) ([]*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, ok := strings.CutPrefix(mode, "launchd:")
	if !ok || key == "" {
		return nil, fmt.Errorf("%q is unsupported on macOS; use launchd:NAME", mode)
	}
	files, err := launchd.Files(key)
	for _, file := range files {
		if file != nil {
			syscall.CloseOnExec(int(file.Fd()))
		}
	}
	return files, err
}

func validateActivatedListening(_ context.Context, fd int) error {
	// launchd calls listen for SockPassive=true and connect otherwise.
	// macOS does not support querying SO_ACCEPTCONN. Reject its active socket
	// alternative without accepting a connection or modifying the shared FD.
	_, err := unix.Getpeername(fd)
	if errors.Is(err, unix.ENOTCONN) {
		return nil
	}
	if err == nil {
		return errors.New("socket activation requires a passive listener, not a connected socket")
	}
	return fmt.Errorf("socket activation inspect passive listener: %w", err)
}

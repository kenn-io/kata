//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/coreos/go-systemd/v22/activation"

	"golang.org/x/sys/unix"
)

func activationFiles(ctx context.Context, mode string) ([]*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode != "systemd" {
		return nil, fmt.Errorf("%q is unsupported on Linux; use systemd", mode)
	}
	return activation.Files(true), nil
}

func validateActivatedListening(_ context.Context, fd int) error {
	listening, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil || listening != 1 {
		return fmt.Errorf("socket activation requires a listening, not connected, socket (error=%v)", err)
	}
	return nil
}

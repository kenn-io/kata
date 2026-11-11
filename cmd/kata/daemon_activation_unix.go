//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/unix"
)

func adoptActivatedListener(ctx context.Context, mode string) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files, err := activationFiles(ctx, mode)
	defer func() {
		for _, file := range files {
			if file != nil {
				_ = file.Close()
			}
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("socket activation: %w", err)
	}
	return adoptActivatedFiles(ctx, files)
}

// adoptActivatedFiles leaves the original descriptors owned by its caller.
func adoptActivatedFiles(ctx context.Context, files []*os.File) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("socket activation requires exactly one Unix stream listener; received %d descriptors", len(files))
	}
	file := files[0]
	if file == nil {
		return nil, errors.New("socket activation: invalid descriptor")
	}
	fd := int(file.Fd())
	unix.CloseOnExec(fd)
	socketType, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || socketType != unix.SOCK_STREAM {
		return nil, fmt.Errorf("socket activation requires a Unix stream socket (type=%d, error=%v)", socketType, err)
	}
	if err := validateActivatedListening(ctx, fd); err != nil {
		return nil, err
	}
	addr, err := unix.Getsockname(fd)
	if err != nil {
		return nil, fmt.Errorf("socket activation getsockname: %w", err)
	}
	pathname, ok := addr.(*unix.SockaddrUnix)
	if !ok || !filepath.IsAbs(pathname.Name) {
		return nil, errors.New("socket activation requires a pathname Unix listener")
	}
	if err := safefileio.ValidatePrivateDir(filepath.Dir(pathname.Name)); err != nil {
		return nil, fmt.Errorf("socket activation requires a private socket parent: %w", err)
	}
	info, err := os.Lstat(pathname.Name)
	if err != nil {
		return nil, fmt.Errorf("socket activation socket path: %w", err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("socket activation path must be a socket owned by the current user")
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("socket activation adopt: %w", err)
	}
	adopted, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		return nil, errors.New("socket activation requires a Unix listener")
	}
	adopted.SetUnlinkOnClose(false)
	return adopted, nil
}

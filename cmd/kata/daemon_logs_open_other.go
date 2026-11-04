//go:build !windows

package main

import (
	"context"
	"os"
)

func openLogFileForTail(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.Open(path) //nolint:gosec // G304: daemon-controlled log path
}

package main

import (
	"context"
	"os"

	"go.kenn.io/kit/safefileio"
)

func openAgentsViewSelectedFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Uses nonblocking/no-follow opens on Unix and verified regular owner file
	// handles on Windows. A FIFO cannot delay the preflight before validation.
	return safefileio.OpenCurrentUserFile(path)
}

package main

import (
	"context"
	"errors"
	"io"
	"os"
)

var errCronInputTooLarge = errors.New("cron input exceeds its byte limit")

// readCronInput shares bounded, cancellable reads while callers own messages
// and exit-code mapping. Only an explicitly opened input file is owned here.
func readCronInput(ctx context.Context, stdin io.Reader, path string, limit int64) (raw []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader := stdin
	if path != "-" {
		file, err := os.Open(path) //nolint:gosec // Explicit cron/conformance input path.
		if err != nil {
			return nil, err
		}
		reader = file
		defer func() { err = errors.Join(err, file.Close()) }()
	}
	// Closing a pipe interrupts ReadAll without retaining a reader goroutine.
	if closer, ok := reader.(io.ReadCloser); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	raw, err = io.ReadAll(io.LimitReader(reader, limit+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errCronInputTooLarge
	}
	return raw, nil
}

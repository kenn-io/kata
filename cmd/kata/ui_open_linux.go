//go:build linux

package main

import (
	"context"
	"os/exec"
)

func platformOpenTarget(ctx context.Context, target string) error {
	return exec.CommandContext(ctx, "xdg-open", target).Run() //nolint:gosec // Target is a validated URL passed as one argument, never through a shell.
}

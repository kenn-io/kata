package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var errOmpUnavailable = errors.New("omp not found on PATH; install omp or use --scope project")

// Ask omp rather than duplicating its profile, override and XDG precedence.
var runOmp = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	binary, err := exec.LookPath("omp")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...) //nolint:gosec // G204: operator-installed omp receives fixed config query arguments.
	command.Dir = dir
	return command.Output()
}

func resolveOmpUserRoot(ctx context.Context, home string) (string, error) {
	output, err := runOmp(ctx, home, "config", "path")
	if err != nil {
		return "", errOmpUnavailable
	}
	var root string
	for line := range strings.SplitSeq(string(output), "\n") {
		if strings.TrimSpace(line) != "" {
			root = strings.TrimSuffix(line, "\r")
		}
	}
	if !filepath.IsAbs(root) {
		return "", errOmpUnavailable
	}
	return root, nil
}

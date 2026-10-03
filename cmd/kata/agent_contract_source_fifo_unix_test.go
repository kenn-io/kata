//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestAgentContractSourceRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.pipe")
	require.NoError(t, unix.Mkfifo(path, 0600))
	for _, source := range []string{path, "prompt.pipe"} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentContractSourceFIFOHelper$") //nolint:gosec // G204: controlled native fixture command or current test executable, with fixed test arguments.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "KATA_TEST_FIFO_SOURCE="+source)
		output, err := cmd.CombinedOutput()
		cancel()
		require.NoError(t, err, "source %q: %s", source, output)
	}
}

func TestAgentContractSourceFIFOHelper(t *testing.T) {
	source := os.Getenv("KATA_TEST_FIFO_SOURCE")
	if source == "" {
		t.Skip("subprocess helper")
	}
	_, err := readAgentContractSource(source, true)
	require.ErrorContains(t, err, "regular UTF-8 text file")
}

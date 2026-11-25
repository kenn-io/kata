package main

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Reproduce kenn-io/kata#1315 with a real refused rename-over: an ACL that
// denies deleting AGENTS.md makes rename over it fail while writes succeed.
func TestAgentInstructionsInstallsOverUndeletableAgentsFile(t *testing.T) {
	home := t.TempDir()
	instructions := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(instructions, []byte("# Runtime managed\n"), 0o600))
	current, err := user.Current()
	require.NoError(t, err)
	if out, err := exec.CommandContext(t.Context(), "chmod", "+a", current.Username+" deny delete", instructions).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed command with a test-owned path.
		t.Skipf("cannot set ACL: %v: %s", err, out)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "chmod", "-N", instructions).Run() //nolint:gosec // G204: fixed command with a test-owned path.
	})
	// The ACL must refuse atomic replacement for this process.
	err = atomicReplaceInstruction(instructions, []byte("# Replaced\n"), 0o600)
	require.True(t, renameOverRefused(instructions, err), "expected a refused rename, got %v", err)
	before, err := os.Stat(instructions)
	require.NoError(t, err)

	artifacts, err := planInstructionArtifacts(home, "example-agent", "install")
	require.NoError(t, err)
	require.NoError(t, applyInstructionArtifacts(artifacts))
	for _, a := range artifacts {
		data, err := os.ReadFile(a.Path) //nolint:gosec // G304: artifact path under TempDir.
		require.NoError(t, err)
		require.Equal(t, a.Content, string(data))
	}
	after, err := os.Stat(instructions)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
}

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Reproduce kenn-io/kata#1315 with a real refused rename-over: an ACL that
// denies deleting AGENTS.md makes rename over it fail while writes succeed.
func TestAgentInstructionsInstallsOverUndeletableAgentsFile(t *testing.T) {
	home := t.TempDir()
	instructions := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(instructions, []byte("# Runtime managed\n"), 0o600))
	user := os.Getenv("USER")
	if user == "" {
		t.Skip("USER unset")
	}
	if out, err := exec.Command("chmod", "+a", user+" deny delete", instructions).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed command with a test-owned path.
		t.Skipf("cannot set ACL: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("chmod", "-N", instructions).Run() //nolint:gosec // G204: fixed command with a test-owned path.
	})
	artifacts, err := planInstructionArtifacts(home, "example-agent", "install")
	require.NoError(t, err)
	require.NoError(t, applyInstructionArtifacts(artifacts))
	for _, a := range artifacts {
		data, err := os.ReadFile(a.Path) //nolint:gosec // G304: artifact path under TempDir.
		require.NoError(t, err)
		require.Equal(t, a.Content, string(data))
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Product contract: the docs Kata serves tell agents to claim with a time limit.
func TestClaimDocsUseTTL(t *testing.T) {
	for _, path := range []string{
		"docs/get-started/quickstart.md",
		"docs/workflows/agents.md",
		"docs/operations/agent-orchestration.md",
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
		require.NoError(t, err)
		assertClaimsCarryTTL(t, path, string(raw))
	}
}

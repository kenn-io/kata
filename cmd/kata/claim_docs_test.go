package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Product contract: the README, docs, and website tell agents to
// claim with a time limit, and each website page's Markdown and HTML forms
// show the same claim commands.
func TestClaimDocsUseTTL(t *testing.T) {
	commands := map[string][]string{}
	for _, path := range []string{
		"README.md",
		"docs/get-started/quickstart.md",
		"docs/workflows/agents.md",
		"docs/website/index.md",
		"docs/website/index.html",
		"docs/website/guide.md",
		"docs/website/guide/index.html",
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
		require.NoError(t, err)
		found := assertClaimsCarryTTL(t, path, string(raw))
		slices.Sort(found)
		commands[path] = slices.Compact(found)
	}
	assert.Equal(t, commands["docs/website/index.md"], commands["docs/website/index.html"])
	assert.Equal(t, commands["docs/website/guide.md"], commands["docs/website/guide/index.html"])
}

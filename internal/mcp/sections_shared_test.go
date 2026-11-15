package mcpserver

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestLoadedSectionsHydrateAndUnion(t *testing.T) {
	selected := &LoadedSections{}
	options := Options{Client: inertTestClient(t), Scope: NewAllScope(), Actor: "example-agent", Version: "test", LoadedSections: selected}
	first, second := connectRawTestServerWithOptions(t, options), connectRawTestServerWithOptions(t, options)
	for i, session := range []*sdkmcp.ClientSession{first, second} {
		loaded, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: []string{"kata.load_docs", "kata.load_projects"}[i]})
		require.NoError(t, err)
		require.False(t, loaded.IsError)
	}
	// A selected section also configured at startup remains available.
	options.StartupSections = []string{"docs"}
	hydrated := connectRawTestServerWithOptions(t, options)
	listed, err := hydrated.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	require.True(t, names["kata.projects"])
	require.True(t, names["kata.search_docs"])
}

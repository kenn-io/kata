package mcpserver

import (
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestLoadedSectionsHydrateAndUnion(t *testing.T) {
	selected := &LoadedSections{}
	options := Options{Client: inertTestClient(t), Scope: NewAllScope(), Actor: "example-agent", Version: "test", LoadedSections: selected}
	first, second := connectRawTestServerWithOptions(t, options), connectRawTestServerWithOptions(t, options)
	var wg sync.WaitGroup
	for i, session := range []*sdkmcp.ClientSession{first, second} {
		wg.Go(func() {
			loaded, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: []string{"kata.load_docs", "kata.load_projects"}[i]})
			require.NoError(t, err)
			require.False(t, loaded.IsError)
		})
	}
	wg.Wait()
	// A selected section also configured at startup must register once.
	options.StartupSections = []string{"docs"}
	hydrated := connectRawTestServerWithOptions(t, options)
	listed, err := hydrated.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := map[string]int{}
	for _, tool := range listed.Tools {
		names[tool.Name]++
	}
	require.Equal(t, 1, names["kata.projects"])
	require.Equal(t, 1, names["kata.search_docs"])
}

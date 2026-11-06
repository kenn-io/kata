package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// Baselines are 20,934 and 262,670 bytes. Allow 10% growth, rounded up to KB.
const firstCatalogBudget = 24_000
const loadedCatalogBudget = 289_000

func catalogOverBudget(result *sdkmcp.ListToolsResult, budget int) string {
	encoded, _ := json.Marshal(result)
	if len(encoded) <= budget {
		return ""
	}
	type toolSize struct {
		name  string
		bytes int
	}
	sizes := make([]toolSize, 0, len(result.Tools))
	for _, tool := range result.Tools {
		encoded, _ := json.Marshal(tool)
		sizes = append(sizes, toolSize{tool.Name, len(encoded)})
	}
	slices.SortFunc(sizes, func(a, b toolSize) int { return b.bytes - a.bytes })
	var report strings.Builder
	fmt.Fprintf(&report, "tools/list is %d bytes, over the %d budget", len(encoded), budget)
	for _, size := range sizes {
		fmt.Fprintf(&report, "\n%s %d", size.name, size.bytes)
	}
	return report.String()
}

func TestToolCatalogStaysWithinBudget(t *testing.T) {
	for _, test := range []struct {
		name    string
		budget  int
		connect func(*testing.T, Options) *sdkmcp.ClientSession
	}{
		{"first list", firstCatalogBudget, connectRawTestServerWithOptions},
		{"every section loaded", loadedCatalogBudget, connectTestServerWithOptions},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := test.connect(t, defaultCatalogOptions(inertTestClient(t)))
			result, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			require.Empty(t, catalogOverBudget(result, test.budget))
		})
	}
}

func TestToolCatalogBudgetFailsOneByteOver(t *testing.T) {
	fixture := toolSections(false, false)[1]
	catalog := func(padding int) *sdkmcp.ListToolsResult {
		server, err := New(defaultCatalogOptions(inertTestClient(t)))
		require.NoError(t, err)
		// Extend an existing loader so the real catalog can use its full budget.
		addTool(server, fixture.loader, fixture.title, fixture.description+strings.Repeat("x", padding), toolHints(true, false, false),
			func(context.Context, *sdkmcp.CallToolRequest, ToolSectionInput) (*sdkmcp.CallToolResult, ToolSectionOutput, error) {
				return successResult(), ToolSectionOutput{}, nil
			})
		session := connectServerSession(t, server, nil)
		result, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		return result
	}
	base, _ := json.Marshal(catalog(0))
	padding := firstCatalogBudget - len(base)
	require.GreaterOrEqual(t, padding, 0)
	over := catalog(padding + 1)
	report := catalogOverBudget(over, firstCatalogBudget)
	require.Contains(t, report, fixture.loader)
	largest := 0
	for _, tool := range over.Tools {
		encoded, _ := json.Marshal(tool)
		largest = max(largest, len(encoded))
	}
	_, tools, _ := strings.Cut(report, "\n")
	first, _, _ := strings.Cut(tools, "\n")
	require.True(t, strings.HasSuffix(first, fmt.Sprintf(" %d", largest)), "largest tool should be first:\n%s", report)
	require.Empty(t, catalogOverBudget(catalog(padding), firstCatalogBudget))
}

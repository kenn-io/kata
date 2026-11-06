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

func defaultCatalogOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project",
		Actor: "example-agent", Version: "test-version",
	}
}

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
			session := test.connect(t, defaultCatalogOptions(t))
			result, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			require.Empty(t, catalogOverBudget(result, test.budget))
		})
	}
}

func TestToolCatalogBudgetFailsOneByteOver(t *testing.T) {
	catalog := func(description string) *sdkmcp.ListToolsResult {
		server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test-server", Version: "1"}, nil)
		addTool(server, "kata.load_fixture", "Load fixture", description, toolHints(true, false, false),
			func(context.Context, *sdkmcp.CallToolRequest, ToolSectionInput) (*sdkmcp.CallToolResult, ToolSectionOutput, error) {
				return successResult(), ToolSectionOutput{}, nil
			})
		session := connectServerSession(t, server, nil)
		result, err := session.ListTools(t.Context(), nil)
		require.NoError(t, err)
		return result
	}
	base, _ := json.Marshal(catalog("x"))
	padding := firstCatalogBudget - len(base)
	over := catalog(strings.Repeat("x", padding+2))
	require.Contains(t, catalogOverBudget(over, firstCatalogBudget), "kata.load_fixture")
	require.Empty(t, catalogOverBudget(catalog(strings.Repeat("x", padding+1)), firstCatalogBudget))
}

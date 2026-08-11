package mcpserver

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/storageadmin"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestStorageToolsRequireStartupOptIn(t *testing.T) {
	without := connectTestServer(t)
	tools, err := without.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.NotContains(t, toolNames(tools.Tools), "kata.storage_export")
	require.NotContains(t, toolNames(tools.Tools), "kata.storage_import")

	admin, err := storageadmin.New(storageadmin.Config{Root: t.TempDir(), SourceDSN: "source.db", Targets: map[string]string{"restore": "restore.db"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	with := connectTestServerWithOptions(t, Options{Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version", StorageAdmin: admin})
	tools, err = with.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Contains(t, toolNames(tools.Tools), "kata.storage_export")
	require.Contains(t, toolNames(tools.Tools), "kata.storage_import")
	for _, tool := range tools.Tools {
		if tool.Name != "kata.storage_export" {
			continue
		}
		properties := schemaObject(t, tool.InputSchema)["properties"].(map[string]any)
		require.Contains(t, properties, "force")
		require.Contains(t, properties, "confirm")
	}
}

func TestStorageExportIncludesDeletedByDefault(t *testing.T) {
	require.True(t, StorageExportInput{}.includeDeleted())
	include := true
	require.True(t, (StorageExportInput{IncludeDeleted: &include}).includeDeleted())
	include = false
	require.False(t, (StorageExportInput{IncludeDeleted: &include}).includeDeleted())
}

func toolNames(tools []*sdkmcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

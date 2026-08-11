package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLeaseToolsArePublished(t *testing.T) {
	session := connectTestServer(t)
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"kata.lease", "kata.lease_force_release", "kata.lease_status", "kata.lease_steal"} {
		require.True(t, names[name], name)
	}
}

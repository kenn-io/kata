package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronRuntimeInventory(t *testing.T) {
	f := newCronFixture(t)
	catalog, err := f.session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range catalog.Tools {
		if tool.Name == "kata.cron_runtimes" {
			require.Equal(t, "object", schemaObject(t, tool.OutputSchema)["type"])
		}
	}
	initial := cronOK(t, f.session, "cron_runtimes", map[string]any{})
	require.Equal(t, "unknown", initial["inventory"].(map[string]any)["state"])
	require.Empty(t, initial["runtimes"])

	// Conflicting advertisements produce advisory warnings in the same response.
	actor := "example-agent"
	for _, version := range []string{"1.0.0", "2.0.0"} {
		id, err := kataclient.NewCronUID()
		require.NoError(t, err)
		_, err = f.client.BeatCronRuntime(t.Context(), &generated.BeatCronRuntimeRequestOptions{
			PathParams: &generated.BeatCronRuntimePath{ProjectID: f.project.ID, RuntimeUID: id},
			Body:       &generated.BeatCronRuntimeBody{Actor: &actor, Label: "example-worker", Adapter: "example", AdapterVersion: &version},
		})
		require.NoError(t, err)
	}
	before, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	result := cronOK(t, f.session, "cron_runtimes", map[string]any{})
	require.Equal(t, "warming", result["inventory"].(map[string]any)["state"])
	require.Len(t, result["runtimes"], 2)
	warnings := result["warnings"].([]any)
	require.Len(t, warnings, 1)
	warning := warnings[0].(map[string]any)
	require.Equal(t, "adapter_version_skew", warning["code"])
	require.Equal(t, "example", warning["adapter"])
	require.Equal(t, []any{"1.0.0", "2.0.0"}, warning["versions"])
	require.NotEmpty(t, warning["message"])
	after, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "inventory reads must not produce events")
	require.True(t, cronCall(t, f.session, "cron_runtimes", map[string]any{"project": "outside-project"}).IsError)
}

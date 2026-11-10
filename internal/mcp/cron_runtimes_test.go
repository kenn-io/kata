package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronRuntimeInventory(t *testing.T) {
	f := newCronFixture(t)
	catalog, err := f.session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var outputSchema *jsonschema.Schema
	for _, tool := range catalog.Tools {
		if tool.Name == "kata.cron_runtimes" {
			require.NoError(t, json.Unmarshal(mustJSON(t, tool.OutputSchema), &outputSchema))
			break
		}
	}
	require.NotNil(t, outputSchema, "runtime tool must advertise its output schema")
	require.Equal(t, "object", outputSchema.Type)
	resolved, err := outputSchema.Resolve(nil)
	require.NoError(t, err)

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
	result := cronOK(t, f.session, "cron_runtimes", map[string]any{})
	require.NoError(t, resolved.Validate(result))
	require.Len(t, result["runtimes"], 2)
	warnings := result["warnings"].([]any)
	require.Len(t, warnings, 1)
	warning := warnings[0].(map[string]any)
	require.Equal(t, "adapter_version_skew", warning["code"])
	require.Equal(t, "example", warning["adapter"])
	require.ElementsMatch(t, []any{"1.0.0", "2.0.0"}, warning["versions"])
}

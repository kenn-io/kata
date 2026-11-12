package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// MCP tool outputSchema must describe an object, including pointer responses.
func TestCronOutputSchemasAreObjects(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{
		Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project",
		Actor: "example-agent", Version: "test", StartupSections: []string{"cron"},
	})
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range result.Tools {
		if tool.OutputSchema == nil {
			continue
		}
		data, err := json.Marshal(tool.OutputSchema)
		require.NoError(t, err)
		var schema map[string]any
		require.NoError(t, json.Unmarshal(data, &schema))
		require.Equal(t, "object", schema["type"], tool.Name)
	}
}

func TestCronJobInputsScalarOutputSchema(t *testing.T) {
	schema := cronOutputSchema[generated.GetCronJobInputsResponse]("kata.cron_job_inputs")
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	for _, value := range []any{"text", float64(4), true} {
		require.NoError(t, resolved.Validate(map[string]any{"job_uid": "x", "definition_event_uid": "y", "declared_by": "job", "inputs": []any{map[string]any{"name": "x", "type": "int", "source": "job", "value": value}}, "errors": []any{}}))
	}
	require.Error(t, resolved.Validate(map[string]any{"job_uid": "x", "definition_event_uid": "y", "declared_by": "job", "inputs": []any{map[string]any{"name": "x", "type": "int", "source": "job", "value": map[string]any{}}}, "errors": []any{}}))
}

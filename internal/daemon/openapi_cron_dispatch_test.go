package daemon

import (
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
)

func TestOpenAPICronDispatchRequiredNullableLast(t *testing.T) {
	doc := OpenAPIDocument()
	schema := doc.Components.Schemas.Map()["CronDispatchExplanation"]
	require.NotNil(t, schema)
	for _, field := range []string{"last", "last_non_idle"} {
		require.Contains(t, schema.Required, field)
		require.True(t, schema.Properties[field].Nullable)
	}
	value := map[string]any{"job_uid": "01K5ZN7SP700000000000000001", "evaluated_at": "2026-11-04T00:00:00Z", "gate": map[string]any{"reason": "idle"}, "last": nil, "last_non_idle": nil}
	result := &huma.ValidateResult{}
	huma.Validate(doc.Components.Schemas, schema, huma.NewPathBuffer(nil, 0), huma.ModeReadFromServer, value, result)
	require.Empty(t, result.Errors, "fresh-daemon null observation must validate")
}

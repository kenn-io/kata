package daemon

import (
	"encoding/json/v2"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
)

func TestOpenAPIPlanningDatesRequiredNullableResponse(t *testing.T) {
	doc := OpenAPIDocument()
	schema := doc.Components.Schemas.Map()["IssuePlanningDates"]
	require.NotNil(t, schema)
	value := map[string]any{"project_id": float64(1), "issue_uid": "01K5ZN7SP700000000000000001", "revision": float64(1), "scheduled_on": nil, "deadline_on": nil}
	result := &huma.ValidateResult{}
	huma.Validate(doc.Components.Schemas, schema, huma.NewPathBuffer(nil, 0), huma.ModeReadFromServer, value, result)
	require.Empty(t, result.Errors, "actual null response must validate")
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		require.Contains(t, schema.Required, field)
	}
	for _, version := range []string{"3.1", "3.0"} {
		t.Run(version, func(t *testing.T) {
			raw, err := OpenAPIJSONVersion(version)
			require.NoError(t, err)
			var document struct {
				Components struct {
					Schemas map[string]struct {
						Required   []string                  `json:"required"`
						Properties map[string]map[string]any `json:"properties"`
					} `json:"schemas"`
				} `json:"components"`
			}
			require.NoError(t, json.Unmarshal(raw, &document))
			published := document.Components.Schemas["IssuePlanningDates"]
			for _, field := range []string{"scheduled_on", "deadline_on"} {
				require.Contains(t, published.Required, field)
				if version == "3.0" {
					require.Equal(t, true, published.Properties[field]["nullable"])
				} else {
					require.Contains(t, published.Properties[field]["type"], "null")
				}
			}
		})
	}
}

package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// New optional response fields let older daemons remain compatible.
func TestOpenAPICronCapabilitiesFeaturesOptional(t *testing.T) {
	raw, err := OpenAPIYAML()
	require.NoError(t, err)
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
				Required   []string       `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &document))
	schema, ok := document.Components.Schemas["CronCapabilitiesResponseBody"]
	require.True(t, ok)
	require.Contains(t, schema.Properties, "features")
	require.NotContains(t, schema.Required, "features")
}

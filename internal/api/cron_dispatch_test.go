package api

import (
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
)

func TestCronDispatchNullableObservations(t *testing.T) {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	schema := huma.SchemaFromType(registry, reflect.TypeFor[CronDispatchExplanation]())
	raw, err := json.Marshal(CronDispatchExplanation{})
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(raw, &value))
	for _, field := range []string{"last", "last_non_idle"} {
		require.Contains(t, value, field)
		require.Nil(t, value[field])
		require.Contains(t, schema.Required, field)
		require.True(t, schema.Properties[field].Nullable)
	}
}

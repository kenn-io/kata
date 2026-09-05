package daemon

import (
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalAPIJSONRejectsNestedNullArray(t *testing.T) {
	var body struct {
		Items []struct {
			Labels []string `json:"labels"`
		} `json:"items"`
	}

	err := unmarshalAPIJSON([]byte(`{"items":[{"labels":null}]}`), &body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "body.items[0].labels: null is not allowed for an array")
}

func TestAPISchemaRegistryDoesNotChangeHumaArrayDefault(t *testing.T) {
	previous := huma.DefaultArrayNullable
	huma.DefaultArrayNullable = true
	t.Cleanup(func() { huma.DefaultArrayNullable = previous })

	schema := newAPISchemaRegistry().Schema(reflect.TypeFor[[]string](), false, "Labels")

	assert.False(t, schema.Nullable)
	assert.True(t, huma.DefaultArrayNullable)
}

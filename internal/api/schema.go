package api

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// requiredNullableObjectSchema keeps absent objects as explicit nulls in the
// schema and in generated Go responses, whose pointers must omit omitempty.
func requiredNullableObjectSchema[T any](registry huma.Registry) *huma.Schema {
	schema := huma.SchemaFromType(registry, reflect.TypeFor[T]())
	schema.Nullable = true
	schema.Extensions = map[string]any{"x-omitempty": false}
	return schema
}

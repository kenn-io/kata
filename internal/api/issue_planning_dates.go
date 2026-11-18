package api

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
)

// IssuePlanningDates exposes required nullable date objects on the read wire.
type IssuePlanningDates db.IssuePlanningDates

// TransformSchema describes null explicitly; a pointer to an object alone is not nullable
// in Huma's reflected object references. Inline dates also downgrade to OpenAPI
// 3.0 nullable objects without ambiguous nullable reference semantics.
func (IssuePlanningDates) TransformSchema(registry huma.Registry, schema *huma.Schema) *huma.Schema {
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		date := huma.SchemaFromType(registry, reflect.TypeFor[db.IssuePlanningDate]())
		date.Nullable = true
		// The Go generator otherwise adds omitempty to nullable pointers,
		// losing required null fields when typed responses are re-encoded.
		date.Extensions = map[string]any{"x-omitempty": false}
		schema.Properties[field] = date
	}
	return schema
}

// IssuePlanningDatesResponse contains a consistent native date read projection.
type IssuePlanningDatesResponse struct{ Body IssuePlanningDates }

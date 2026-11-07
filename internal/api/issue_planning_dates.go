package api

import (
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
		schema.Properties[field] = requiredNullableObjectSchema[db.IssuePlanningDate](registry)
	}
	return schema
}

// IssuePlanningDatesResponse contains a consistent native date read projection.
type IssuePlanningDatesResponse struct{ Body IssuePlanningDates }

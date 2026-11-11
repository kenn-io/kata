package api

import "github.com/danielgtaylor/huma/v2"

// TransformSchema applies the shared request size and rejects unknown fields.
func (MoveIssuesBody) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	schema.AdditionalProperties = false
	schema.Properties["items"].MaxItems = new(MoveBatchMaxItems)
	return schema
}

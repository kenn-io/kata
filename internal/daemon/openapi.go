package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
)

// APISchemaVersion is the version stamped into the daemon's OpenAPI document
// (info.version). It tracks the HTTP API contract, not the build version, so
// the committed schema artifact stays stable across builds and is bumped
// deliberately when the wire contract changes.
const APISchemaVersion = "0.1.0"

// OpenAPIDocument builds the daemon's complete OpenAPI model by wiring every
// route through NewServer with a zero ServerConfig. It binds no listener and
// needs no database: route handlers capture the config but are never invoked
// here, so the registration alone is enough to materialize the schema. Because
// it reuses NewServer, the emitted document reflects the daemon's real Huma
// configuration — notably the disabled SchemaLinkTransformer — so the schema
// matches the daemon's actual wire shapes.
func OpenAPIDocument() *huma.OpenAPI {
	doc := NewServer(ServerConfig{}).API().OpenAPI()
	applyJSONBlobSchemaOverrides(doc)
	applyArrayQueryParamEncoding(doc)
	return doc
}

// OpenAPIYAML renders the OpenAPI document (OpenAPI 3.1) as YAML.
func OpenAPIYAML() ([]byte, error) {
	return OpenAPIYAMLVersion("3.1")
}

// OpenAPIYAMLVersion renders the OpenAPI document as YAML for a supported
// OpenAPI version. Version 3.0 is used by code generators that do not yet
// consume OpenAPI 3.1's JSON Schema dialect.
func OpenAPIYAMLVersion(version string) ([]byte, error) {
	doc := OpenAPIDocument()
	switch version {
	case "3.1":
		return doc.YAML()
	case "3.0":
		return doc.DowngradeYAML()
	default:
		return nil, fmt.Errorf("unsupported openapi version %q", version)
	}
}

// OpenAPIJSONVersion renders the OpenAPI document as pretty JSON.
func OpenAPIJSONVersion(version string) ([]byte, error) {
	doc := OpenAPIDocument()
	var (
		raw []byte
		err error
	)
	switch version {
	case "3.1":
		raw, err = doc.MarshalJSON()
	case "3.0":
		raw, err = doc.Downgrade()
	default:
		return nil, fmt.Errorf("unsupported openapi version %q", version)
	}
	if err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

func applyJSONBlobSchemaOverrides(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	for name, schema := range doc.Components.Schemas.Map() {
		applyJSONBlobSchemaOverridesTo(name, schema, map[*huma.Schema]struct{}{})
	}
}

func applyJSONBlobSchemaOverridesTo(componentName string, schema *huma.Schema, seen map[*huma.Schema]struct{}) {
	if schema == nil {
		return
	}
	if _, ok := seen[schema]; ok {
		return
	}
	seen[schema] = struct{}{}

	for name, prop := range schema.Properties {
		switch name {
		case "data":
			if componentName == "ErrorBody" {
				schema.Properties[name] = jsonObjectSchema()
				continue
			}
			applyJSONBlobSchemaOverridesTo("", prop, seen)
		case "patch":
			if componentName == "PatchIssueMetadataRequestBody" || componentName == "PatchProjectMetadataRequestBody" {
				schema.Properties[name] = jsonObjectSchema()
				continue
			}
			applyJSONBlobSchemaOverridesTo("", prop, seen)
		case "metadata":
			if componentName == "RecurrenceTemplateUpdateInput" {
				schema.Properties[name] = jsonNullableObjectSchema()
				continue
			}
			schema.Properties[name] = jsonObjectSchema()
		case "template_metadata":
			schema.Properties[name] = jsonObjectSchema()
		case "template_labels":
			schema.Properties[name] = jsonStringArraySchema()
		default:
			applyJSONBlobSchemaOverridesTo("", prop, seen)
		}
	}
	applyJSONBlobSchemaOverridesTo("", schema.Items, seen)
	for _, child := range schema.OneOf {
		applyJSONBlobSchemaOverridesTo("", child, seen)
	}
	for _, child := range schema.AnyOf {
		applyJSONBlobSchemaOverridesTo("", child, seen)
	}
	for _, child := range schema.AllOf {
		applyJSONBlobSchemaOverridesTo("", child, seen)
	}
	applyJSONBlobSchemaOverridesTo("", schema.Not, seen)
}

func applyArrayQueryParamEncoding(doc *huma.OpenAPI) {
	if doc == nil {
		return
	}
	for _, path := range doc.Paths {
		if path == nil {
			continue
		}
		applyArrayQueryParamEncodingTo(path.Parameters)
		for _, op := range []*huma.Operation{
			path.Get,
			path.Put,
			path.Post,
			path.Delete,
			path.Options,
			path.Head,
			path.Patch,
			path.Trace,
		} {
			if op != nil {
				applyArrayQueryParamEncodingTo(op.Parameters)
			}
		}
	}
}

func applyArrayQueryParamEncodingTo(params []*huma.Param) {
	for _, param := range params {
		if param == nil || param.In != "query" || param.Schema == nil || param.Schema.Type != huma.TypeArray {
			continue
		}
		explode := true
		param.Explode = &explode
	}
}

func jsonObjectSchema() *huma.Schema {
	return &huma.Schema{
		Type:                 huma.TypeObject,
		AdditionalProperties: true,
	}
}

func jsonNullableObjectSchema() *huma.Schema {
	schema := jsonObjectSchema()
	schema.Nullable = true
	return schema
}

func jsonStringArraySchema() *huma.Schema {
	return &huma.Schema{
		Type:  huma.TypeArray,
		Items: &huma.Schema{Type: huma.TypeString},
	}
}

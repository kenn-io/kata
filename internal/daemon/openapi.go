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
const APISchemaVersion = "0.5.0"

// OpenAPIDocument builds the daemon's complete OpenAPI model by wiring every
// route through NewServer with a zero ServerConfig. It binds no listener and
// needs no database: route handlers capture the config but are never invoked
// here, so the registration alone is enough to materialize the schema. Because
// it reuses NewServer, the emitted document reflects the daemon's real Huma
// configuration — notably the disabled SchemaLinkTransformer — so the schema
// matches the daemon's actual wire shapes.
func OpenAPIDocument() *huma.OpenAPI {
	doc := baseOpenAPIDocument()
	relaxResponseAdditionalProperties(doc)
	return doc
}

// openAPIClientDocument builds the document flavor consumed by code
// generators (`kata openapi --version 3.0`). Response schemas leave
// additionalProperties unset instead of the published document's explicit
// true: absence carries the same permissive semantics, but the Go client
// generator (oapi-codegen-dd) models optional object-typed properties as
// value types whenever the target schema carries an explicit
// additionalProperties constraint.
func openAPIClientDocument() *huma.OpenAPI {
	doc := baseOpenAPIDocument()
	clearResponseAdditionalProperties(doc)
	return doc
}

func baseOpenAPIDocument() *huma.OpenAPI {
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
// OpenAPI version. Version 3.0 is the code-generator flavor: it serves
// generators that do not yet consume OpenAPI 3.1's JSON Schema dialect, and
// its response schemas leave additionalProperties unset (see
// openAPIClientDocument).
func OpenAPIYAMLVersion(version string) ([]byte, error) {
	switch version {
	case "3.1":
		return OpenAPIDocument().YAML()
	case "3.0":
		return openAPIClientDocument().DowngradeYAML()
	default:
		return nil, fmt.Errorf("unsupported openapi version %q", version)
	}
}

// OpenAPIJSONVersion renders the OpenAPI document as pretty JSON. The 3.0
// flavor differs from 3.1 the same way as in OpenAPIYAMLVersion.
func OpenAPIJSONVersion(version string) ([]byte, error) {
	var (
		raw []byte
		err error
	)
	switch version {
	case "3.1":
		raw, err = OpenAPIDocument().MarshalJSON()
	case "3.0":
		raw, err = openAPIClientDocument().Downgrade()
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

// relaxResponseAdditionalProperties lets response bodies carry fields a client's
// generated schema does not yet know about. Huma stamps additionalProperties:false
// on every struct it emits, which would make a strict client validator reject
// additive response fields — contradicting the compatibility policy in
// docs/reference/http-api.md, where additive optional response fields may appear
// without an api_schema_version bump. It walks every response schema and flips
// that strict default to an explicit additionalProperties:true, while leaving
// request schemas untouched so request validation is unchanged.
func relaxResponseAdditionalProperties(doc *huma.OpenAPI) {
	replaceStrictResponseAdditionalProperties(doc, true)
}

// clearResponseAdditionalProperties removes the strict default instead of
// flipping it to true. Used by the code-generator document flavor: an unset
// additionalProperties is just as permissive, and it keeps generators from
// modeling optional object-typed properties as value types (see
// openAPIClientDocument).
func clearResponseAdditionalProperties(doc *huma.OpenAPI) {
	replaceStrictResponseAdditionalProperties(doc, nil)
}

// replaceStrictResponseAdditionalProperties rewrites the
// additionalProperties:false default on every response-reachable schema to
// replacement. Schemas reachable from a request body are skipped so request
// validation never loosens; map-typed schemas (additionalProperties holding a
// value schema) are untouched.
func replaceStrictResponseAdditionalProperties(doc *huma.OpenAPI, replacement any) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	reg := doc.Components.Schemas
	ops := documentOperations(doc)
	requestStrict := requestReachableSchemas(ops, reg)

	seen := map[*huma.Schema]struct{}{}
	for _, op := range ops {
		for _, resp := range op.Responses {
			for _, mt := range resp.Content {
				walkSchemaTree(mt.Schema, reg, seen, func(schema *huma.Schema) {
					if _, ok := requestStrict[schema]; ok {
						return
					}
					if ap, ok := schema.AdditionalProperties.(bool); ok && !ap {
						schema.AdditionalProperties = replacement
					}
				})
			}
		}
	}
}

// requestReachableSchemas returns every schema reachable from a request body.
// These are excluded from relaxation so a schema shared between a request and a
// response is never silently loosened; today the graphs are disjoint, but this
// keeps the relaxation pass sound if they ever cross.
func requestReachableSchemas(ops []*huma.Operation, reg huma.Registry) map[*huma.Schema]struct{} {
	strict := map[*huma.Schema]struct{}{}
	seen := map[*huma.Schema]struct{}{}
	for _, op := range ops {
		if op.RequestBody == nil {
			continue
		}
		for _, mt := range op.RequestBody.Content {
			walkSchemaTree(mt.Schema, reg, seen, func(schema *huma.Schema) {
				strict[schema] = struct{}{}
			})
		}
	}
	return strict
}

// documentOperations returns every operation defined across the document's paths.
func documentOperations(doc *huma.OpenAPI) []*huma.Operation {
	var ops []*huma.Operation
	for _, item := range doc.Paths {
		if item == nil {
			continue
		}
		for _, op := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if op != nil {
				ops = append(ops, op)
			}
		}
	}
	return ops
}

// walkSchemaTree visits schema and every schema reachable from it, resolving
// component $refs through reg and guarding against cycles with seen.
func walkSchemaTree(
	schema *huma.Schema,
	reg huma.Registry,
	seen map[*huma.Schema]struct{},
	visit func(*huma.Schema),
) {
	if schema == nil {
		return
	}
	if schema.Ref != "" {
		walkSchemaTree(reg.SchemaFromRef(schema.Ref), reg, seen, visit)
		return
	}
	if _, ok := seen[schema]; ok {
		return
	}
	seen[schema] = struct{}{}
	visit(schema)
	for _, child := range schemaChildren(schema) {
		walkSchemaTree(child, reg, seen, visit)
	}
}

// schemaChildren returns the subschemas directly nested in schema. Nil entries
// (an absent items or not) are harmless: walkSchemaTree skips them.
func schemaChildren(schema *huma.Schema) []*huma.Schema {
	children := make([]*huma.Schema, 0, len(schema.Properties)+len(schema.OneOf)+len(schema.AnyOf)+len(schema.AllOf)+3)
	for _, prop := range schema.Properties {
		children = append(children, prop)
	}
	children = append(children, schema.Items, schema.Not)
	if sub, ok := schema.AdditionalProperties.(*huma.Schema); ok {
		children = append(children, sub)
	}
	children = append(children, schema.OneOf...)
	children = append(children, schema.AnyOf...)
	children = append(children, schema.AllOf...)
	return children
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
		case "config":
			if componentName == "EnableIssueSyncRequestBody" || componentName == "IssueSyncBindingOut" {
				schema.Properties[name] = jsonObjectSchema()
				continue
			}
			applyJSONBlobSchemaOverridesTo("", prop, seen)
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

package daemon

import (
	"encoding/json/jsontext"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
)

// APISchemaVersion is the version stamped into the daemon's OpenAPI document
// (info.version). It tracks the HTTP API contract, not the build version, so
// the committed schema artifact stays stable across builds and is bumped
// deliberately when the wire contract changes.
const APISchemaVersion = "0.26.0"

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
	allowEmptyFederationJoinClientStrings(doc)
	return doc
}

// Required JSON properties specify presence, not a nonempty string. These
// fields are intentionally empty when no origin is configured or a grant
// cannot support a runnable join. Override the Go generator's inferred
// nonzero-value validation without making the wire properties optional.
func allowEmptyFederationJoinClientStrings(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	join := doc.Components.Schemas.Map()["FederationJoinInstructions"]
	if join == nil {
		return
	}
	for _, name := range []string{"hub_url", "join_command"} {
		property := join.Properties[name]
		if property == nil {
			continue
		}
		if property.Extensions == nil {
			property.Extensions = map[string]any{}
		}
		property.Extensions["x-oapi-codegen-extra-tags"] = map[string]any{"validate": "omitempty"}
	}
}

func baseOpenAPIDocument() *huma.OpenAPI {
	doc := NewServer(ServerConfig{}).API().OpenAPI()
	applyDocumentPostProcessing(doc)
	return doc
}

func applyMetadataPatchGuardSchema(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	guard := doc.Components.Schemas.Map()["MetadataPatchGuard"]
	if guard == nil {
		return
	}
	variant := func(condition string, conditionSchema *huma.Schema) *huma.Schema {
		return &huma.Schema{
			Type:                 huma.TypeObject,
			AdditionalProperties: false,
			Properties: map[string]*huma.Schema{
				"key":     {Type: huma.TypeString},
				condition: conditionSchema,
			},
			Required: []string{"key", condition},
		}
	}
	*guard = huma.Schema{OneOf: []*huma.Schema{
		variant("if_value", &huma.Schema{
			Type:        huma.TypeString,
			Description: "Expected metadata value encoded as JSON text",
		}),
		variant("if_absent", &huma.Schema{Type: huma.TypeBoolean, Enum: []any{true}}),
	}}
	guard.OneOf[0].PrecomputeMessages()
	guard.OneOf[1].PrecomputeMessages()
	guard.PrecomputeMessages()
}

// applyDocumentPostProcessing runs the document-level passes that shape the
// daemon's OpenAPI output. Opaque JSON wire shapes come from the types
// themselves (huma.SchemaProvider — see internal/api/jsonwire.go and
// internal/db/types.go), so adding one cannot silently publish the wrong
// schema and a typed property named "metadata" keeps its reflected shape.
func applyDocumentPostProcessing(doc *huma.OpenAPI) {
	applyMetadataPatchGuardSchema(doc)
	applyArrayQueryParamEncoding(doc)
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
	pretty := jsontext.Value(raw)
	if err := pretty.Indent(jsontext.WithIndent("  ")); err != nil {
		return nil, err
	}
	return append(pretty, '\n'), nil
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

// replaceStrictResponseAdditionalProperties separates shared request schemas
// before relaxing response graphs, preserving strict request validation.
func replaceStrictResponseAdditionalProperties(doc *huma.OpenAPI, replacement any) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	reg := doc.Components.Schemas
	ops := documentOperations(doc)
	strict := requestReachableSchemas(ops, reg)
	copies := map[*huma.Schema]*huma.Schema{}
	refs := map[string]string{}
	var responseSchema func(*huma.Schema) *huma.Schema
	responseSchema = func(source *huma.Schema) *huma.Schema {
		if source == nil {
			return nil
		}
		if found, ok := copies[source]; ok {
			return found
		}
		target := source
		if source.Ref != "" {
			resolved := reg.SchemaFromRef(source.Ref)
			if _, shared := strict[resolved]; shared {
				ref, exists := refs[source.Ref]
				if !exists {
					name := strings.TrimPrefix(source.Ref, "#/components/schemas/") + "Response"
					base := name
					for suffix := 2; reg.Map()[name] != nil; suffix++ {
						name = base + strconv.Itoa(suffix)
					}
					ref = "#/components/schemas/" + name
					refs[source.Ref] = ref
					// Reserve the component before walking recursive references.
					reg.Map()[name] = &huma.Schema{}
					reg.Map()[name] = responseSchema(resolved)
				}
				cloned := *source
				cloned.Ref = ref
				target = &cloned
			} else {
				responseSchema(resolved)
			}
			copies[source] = target
			return target
		}
		if _, shared := strict[source]; shared {
			cloned := *source
			target = &cloned
		}
		copies[source] = target
		if ap, ok := target.AdditionalProperties.(bool); ok && !ap {
			target.AdditionalProperties = replacement
		}
		properties := make(map[string]*huma.Schema, len(source.Properties))
		for key, child := range source.Properties {
			properties[key] = responseSchema(child)
		}
		if source.Properties != nil {
			target.Properties = properties
		}
		target.Items = responseSchema(source.Items)
		target.Not = responseSchema(source.Not)
		if child, ok := source.AdditionalProperties.(*huma.Schema); ok {
			target.AdditionalProperties = responseSchema(child)
		}
		children := func(input []*huma.Schema) []*huma.Schema {
			if input == nil {
				return nil
			}
			output := make([]*huma.Schema, len(input))
			for i, child := range input {
				output[i] = responseSchema(child)
			}
			return output
		}
		target.OneOf = children(source.OneOf)
		target.AnyOf = children(source.AnyOf)
		target.AllOf = children(source.AllOf)
		return target
	}
	for _, op := range ops {
		for _, resp := range op.Responses {
			for _, mt := range resp.Content {
				mt.Schema = responseSchema(mt.Schema)
			}
		}
	}
}

// requestReachableSchemas returns every schema reachable from a request body.
// Response uses of these schemas receive separate component definitions.
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

// applyErrorEnvelopeResponses replaces Huma's process-global default error
// model in this API's document only. Runtime framework errors are converted by
// api.TransformHumaError; keeping the same transformation local here preserves
// the published wire contract without changing other Huma APIs in the process.
func applyErrorEnvelopeResponses(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}

	registry := doc.Components.Schemas
	errorSchema := registry.Schema(reflect.TypeFor[api.ErrorEnvelope](), true, "ErrorEnvelope")
	for _, operation := range documentOperations(doc) {
		for code, response := range operation.Responses {
			status, err := strconv.Atoi(code)
			if code != "default" && (err != nil || status < 400) {
				continue
			}
			response.Content = map[string]*huma.MediaType{
				"application/json": {Schema: errorSchema},
			}
		}
	}

	// These framework schemas are no longer referenced after every error
	// response is rewritten. Do not leak unrelated Huma error components into
	// Kata's committed API artifact.
	delete(registry.Map(), "ErrorDetail")
	delete(registry.Map(), "ErrorModel")
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

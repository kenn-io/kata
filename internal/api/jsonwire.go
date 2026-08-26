package api //nolint:revive // package name "api" is fixed by Plan 1 §4 wire-types layout.

import (
	"encoding/json"
	"errors"

	"github.com/danielgtaylor/huma/v2"
)

// This file holds the wire types for fields whose contract is "an arbitrary
// JSON object". Each one implements huma.SchemaProvider so its declaration
// and its published OpenAPI schema are the same fact in one place. Before
// this existed, internal/daemon rebuilt these schemas by string-matching JSON
// property names on the finished document, so adding an opaque-JSON field
// here published the wrong schema unless someone remembered to edit a table
// in another package.
//
// additionalProperties is explicitly true rather than left unset: the
// code-generator document flavor clears it where oapi-codegen needs it
// cleared (see openAPIClientDocument in internal/daemon/openapi.go).

// jsonObjectSchema is the shared "arbitrary JSON object" schema. Each call
// returns a fresh value: huma stores the returned pointer in the document,
// and later document passes may mutate it in place.
func jsonObjectSchema() *huma.Schema {
	return &huma.Schema{
		Type:                 huma.TypeObject,
		AdditionalProperties: true,
	}
}

// JSONMap is an opaque JSON object decoded into Go values — used for
// provider-specific issue-sync configuration and for the error envelope's
// structured data.
type JSONMap map[string]any

// Schema implements huma.SchemaProvider.
func (JSONMap) Schema(huma.Registry) *huma.Schema { return jsonObjectSchema() }

// JSONRawMap is an opaque JSON object whose values stay undecoded, so each
// key can be validated (or passed through) on its own terms. Used by the
// create-issue metadata body and by both metadata patch bodies.
type JSONRawMap map[string]json.RawMessage

// Schema implements huma.SchemaProvider.
func (JSONRawMap) Schema(huma.Registry) *huma.Schema { return jsonObjectSchema() }

// JSONRawObject is an opaque JSON object carried as undecoded bytes.
// MarshalJSON/UnmarshalJSON reproduce json.RawMessage's behavior exactly — a
// defined type over []byte inherits no methods, so without these the value
// would be base64-encoded on the wire.
type JSONRawObject json.RawMessage

// MarshalJSON emits the stored bytes verbatim; the empty value emits null.
func (m JSONRawObject) MarshalJSON() ([]byte, error) {
	if len(m) == 0 {
		return []byte("null"), nil
	}
	return m, nil
}

// UnmarshalJSON copies the raw input bytes verbatim.
func (m *JSONRawObject) UnmarshalJSON(data []byte) error {
	if m == nil {
		return errors.New("api.JSONRawObject: UnmarshalJSON on nil pointer")
	}
	*m = append((*m)[0:0], data...)
	return nil
}

// Schema implements huma.SchemaProvider.
func (JSONRawObject) Schema(huma.Registry) *huma.Schema { return jsonObjectSchema() }

// JSONNullableRawObject is JSONRawObject for fields that additionally accept
// an explicit JSON null. It is a distinct type because huma resolves a
// SchemaProvider on the dereferenced type, so pointer-ness alone cannot make
// a self-describing schema nullable.
type JSONNullableRawObject json.RawMessage

// MarshalJSON emits the stored bytes verbatim; the empty value emits null.
func (m JSONNullableRawObject) MarshalJSON() ([]byte, error) {
	return JSONRawObject(m).MarshalJSON()
}

// UnmarshalJSON copies the raw input bytes verbatim.
func (m *JSONNullableRawObject) UnmarshalJSON(data []byte) error {
	return (*JSONRawObject)(m).UnmarshalJSON(data)
}

// Schema implements huma.SchemaProvider.
func (JSONNullableRawObject) Schema(huma.Registry) *huma.Schema {
	schema := jsonObjectSchema()
	schema.Nullable = true
	return schema
}

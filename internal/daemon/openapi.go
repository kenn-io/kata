package daemon

import "github.com/danielgtaylor/huma/v2"

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
	return NewServer(ServerConfig{}).API().OpenAPI()
}

// OpenAPIYAML renders the OpenAPI document (OpenAPI 3.1) as YAML.
func OpenAPIYAML() ([]byte, error) {
	return OpenAPIDocument().YAML()
}

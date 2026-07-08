package daemon

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
)

// artifactPath is the committed OpenAPI schema, relative to this package dir.
// A const (not a var) keeps gosec G304 quiet: the path is fixed, not caller-supplied.
const artifactPath = "../../api/openapi.yaml"
const clientSpecArtifactPath = "../../pkg/client/openapi.yaml"
const clientGeneratedDir = "../../pkg/client/generated"

// TestOpenAPIArtifactUpToDate fails if the committed api/openapi.yaml no longer
// matches the schema generated from the current routes. Regenerate with
// `make openapi`.
func TestOpenAPIArtifactUpToDate(t *testing.T) {
	got, err := OpenAPIYAML()
	if err != nil {
		t.Fatalf("OpenAPIYAML: %v", err)
	}
	want, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `make openapi` to generate it)", artifactPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; run `make api-generate` to regenerate", artifactPath)
	}
}

func TestOpenAPIClientSpecArtifactUpToDate(t *testing.T) {
	got, err := OpenAPIYAMLVersion("3.0")
	if err != nil {
		t.Fatalf("OpenAPIYAMLVersion(3.0): %v", err)
	}
	want, err := os.ReadFile(clientSpecArtifactPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `make api-generate` to generate it)", clientSpecArtifactPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; run `make api-generate` to regenerate", clientSpecArtifactPath)
	}
}

func TestOpenAPIClientArtifactUpToDate(t *testing.T) {
	tmpRoot := t.TempDir()
	tmpGenerated := filepath.Join(tmpRoot, "generated")
	if err := os.Mkdir(tmpGenerated, 0o700); err != nil {
		t.Fatalf("mkdir generated temp dir: %v", err)
	}
	config, err := os.ReadFile(filepath.Join(clientGeneratedDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	tmpConfig := filepath.Join(tmpGenerated, "config.yaml")
	if err := os.WriteFile(tmpConfig, config, 0o600); err != nil { //nolint:gosec // test-controlled path under t.TempDir
		t.Fatalf("write generated config: %v", err)
	}
	if err := copyGeneratedTemplates(filepath.Join(clientGeneratedDir, "templates"), filepath.Join(tmpGenerated, "templates")); err != nil {
		t.Fatalf("copy generated templates: %v", err)
	}
	tmpSpec := filepath.Join(tmpRoot, "openapi.yaml")
	spec, err := os.ReadFile(clientSpecArtifactPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `make api-generate` to generate it)", clientSpecArtifactPath, err)
	}
	if err := os.WriteFile(tmpSpec, spec, 0o600); err != nil { //nolint:gosec // test-controlled path under t.TempDir
		t.Fatalf("write generated spec: %v", err)
	}

	cmd := exec.Command("go", "run", "github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.5", "-config", "config.yaml", "../openapi.yaml")
	cmd.Dir = tmpGenerated
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generate client: %v\n%s", err, out)
	}

	gotFiles, err := generatedGoFiles(tmpGenerated)
	if err != nil {
		t.Fatalf("list generated temp files: %v", err)
	}
	wantFiles, err := generatedGoFiles(clientGeneratedDir)
	if err != nil {
		t.Fatalf("list %s: %v (run `make api-generate` to generate it)", clientGeneratedDir, err)
	}
	if len(gotFiles) != len(wantFiles) {
		t.Fatalf("%s is stale; generated %d Go files, want %d", clientGeneratedDir, len(gotFiles), len(wantFiles))
	}
	for i := range wantFiles {
		if gotFiles[i] != wantFiles[i] {
			t.Fatalf("%s is stale; generated file %q, want %q", clientGeneratedDir, gotFiles[i], wantFiles[i])
		}
		got, err := os.ReadFile(filepath.Join(tmpGenerated, gotFiles[i])) //nolint:gosec // generated filename is enumerated from t.TempDir output
		if err != nil {
			t.Fatalf("read generated temp file %s: %v", gotFiles[i], err)
		}
		want, err := os.ReadFile(filepath.Join(clientGeneratedDir, wantFiles[i]))
		if err != nil {
			t.Fatalf("read %s: %v (run `make api-generate` to regenerate)", filepath.Join(clientGeneratedDir, wantFiles[i]), err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s is stale; run `make api-generate` to regenerate", filepath.Join(clientGeneratedDir, wantFiles[i]))
		}
	}
}

func copyGeneratedTemplates(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path) //nolint:gosec // generated template filename is enumerated from a fixed test fixture dir
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600) //nolint:gosec // test-controlled copy under t.TempDir
	})
}

func generatedGoFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || entry.Name() == "generate.go" {
			continue
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	return files, nil
}

// TestOpenAPIYAMLDeterministic guards against map-ordering nondeterminism that
// would make the committed artifact churn between generations.
func TestOpenAPIYAMLDeterministic(t *testing.T) {
	first, err := OpenAPIYAML()
	if err != nil {
		t.Fatalf("OpenAPIYAML (first): %v", err)
	}
	second, err := OpenAPIYAML()
	if err != nil {
		t.Fatalf("OpenAPIYAML (second): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("OpenAPIYAML is nondeterministic across calls")
	}
}

// TestOpenAPIDocumentShape sanity-checks the generated document.
func TestOpenAPIDocumentShape(t *testing.T) {
	doc := OpenAPIDocument()
	if doc.OpenAPI == "" {
		t.Error("missing openapi version string")
	}
	if doc.Info == nil || doc.Info.Version != APISchemaVersion {
		t.Errorf("info.version = %+v, want %s", doc.Info, APISchemaVersion)
	}
	if len(doc.Paths) == 0 {
		t.Error("no paths registered in document")
	}
}

func TestAPISchemaVersionReflectsBranchOrchestrationContract(t *testing.T) {
	if APISchemaVersion != "0.5.0" {
		t.Fatalf("APISchemaVersion = %q, want 0.5.0 for create metadata and list metadata filter contract", APISchemaVersion)
	}
}

func TestOpenAPIDocumentIncludesEventsStream(t *testing.T) {
	doc := OpenAPIDocument()
	path := doc.Paths["/api/v1/events/stream"]
	if path == nil || path.Get == nil {
		t.Fatal("missing GET /api/v1/events/stream")
	}
	resp := path.Get.Responses["200"]
	if resp == nil {
		t.Fatal("missing 200 response for GET /api/v1/events/stream")
	}
	if resp.Content["text/event-stream"] == nil {
		t.Fatal("missing text/event-stream response content")
	}
}

func TestOpenAPIDocumentJSONBlobShapes(t *testing.T) {
	doc := OpenAPIDocument()
	assertSchemaPropertyType(t, doc, "Issue", "metadata", huma.TypeObject)
	// The create body's optional metadata is an open object (the generic
	// `metadata` override), so callers can supply arbitrary keys.
	assertSchemaPropertyType(t, doc, "CreateIssueRequestBody", "metadata", huma.TypeObject)
	createMeta := doc.Components.Schemas.Map()["CreateIssueRequestBody"].Properties["metadata"]
	if createMeta.AdditionalProperties != true {
		t.Fatalf("CreateIssueRequestBody.metadata additionalProperties = %#v, want true", createMeta.AdditionalProperties)
	}
	assertSchemaPropertyType(t, doc, "ProjectOut", "metadata", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "ReadyGlobalIssue", "metadata", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "Recurrence", "template_labels", huma.TypeArray)
	assertSchemaPropertyType(t, doc, "Recurrence", "template_metadata", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "RecurrenceTemplateUpdateInput", "metadata", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "EnableIssueSyncRequestBody", "config", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "IssueSyncBindingOut", "config", huma.TypeObject)

	labels := doc.Components.Schemas.Map()["Recurrence"].Properties["template_labels"]
	if labels.Items == nil || labels.Items.Type != huma.TypeString {
		t.Fatalf("Recurrence.template_labels items = %+v, want string items", labels.Items)
	}
	updateMetadata := doc.Components.Schemas.Map()["RecurrenceTemplateUpdateInput"].Properties["metadata"]
	if !updateMetadata.Nullable {
		t.Fatal("RecurrenceTemplateUpdateInput.metadata must allow null")
	}
	for _, schemaName := range []string{"EnableIssueSyncRequestBody", "IssueSyncBindingOut"} {
		config := doc.Components.Schemas.Map()[schemaName].Properties["config"]
		if config.AdditionalProperties != true {
			t.Fatalf("%s.config additionalProperties = %#v, want true", schemaName, config.AdditionalProperties)
		}
	}
}

func TestOpenAPIDocumentArrayQueryParamsExplode(t *testing.T) {
	doc := OpenAPIDocument()
	op := doc.Paths["/api/v1/digest"].Get
	if op == nil {
		t.Fatal("missing GET /api/v1/digest")
	}
	var actor *huma.Param
	for _, param := range op.Parameters {
		if param.Name == "actor" && param.In == "query" {
			actor = param
			break
		}
	}
	if actor == nil {
		t.Fatal("missing digest actor query parameter")
	}
	if actor.Schema == nil || actor.Schema.Type != huma.TypeArray {
		t.Fatalf("digest actor schema = %+v, want array", actor.Schema)
	}
	if actor.Explode == nil || !*actor.Explode {
		t.Fatalf("digest actor explode = %v, want true", actor.Explode)
	}
}

// TestResponseBodyAllowsAdditionalProperties pins the response side of the
// documented compatibility policy (docs/reference/http-api.md): additive
// optional response fields may appear without an api_schema_version bump, so a
// response schema must not reject unknown fields. HealthResponseBody is a pure
// response body, so it must permit additional properties.
func TestResponseBodyAllowsAdditionalProperties(t *testing.T) {
	doc := OpenAPIDocument()
	schema := doc.Components.Schemas.Map()["HealthResponseBody"]
	require.NotNil(t, schema, "missing HealthResponseBody schema")
	require.Equal(t, true, schema.AdditionalProperties, "HealthResponseBody must allow unknown fields")
}

// TestRequestBodyRejectsAdditionalProperties guards the request side: relaxing
// responses must not loosen request validation. The daemon should keep
// rejecting unknown request fields, so request bodies stay strict.
func TestRequestBodyRejectsAdditionalProperties(t *testing.T) {
	doc := OpenAPIDocument()
	schema := doc.Components.Schemas.Map()["CreateIssueRequestBody"]
	require.NotNil(t, schema, "missing CreateIssueRequestBody schema")
	require.Equal(t, false, schema.AdditionalProperties, "request bodies must reject unknown fields")
}

// TestAllResponseSchemasAllowAdditionalProperties is the policy invariant: every
// object schema reachable from any response must permit unknown fields, so
// additive response evolution never breaks a client that strict-validates
// against the published schema. It walks responses independently of the
// production relaxation pass to cross-check that pass's coverage.
func TestAllResponseSchemasAllowAdditionalProperties(t *testing.T) {
	doc := OpenAPIDocument()
	reg := doc.Components.Schemas
	seen := map[*huma.Schema]struct{}{}
	var strict []string

	var walk func(name string, schema *huma.Schema)
	walk = func(name string, schema *huma.Schema) {
		if schema == nil {
			return
		}
		if schema.Ref != "" {
			walk(refName(schema.Ref), reg.SchemaFromRef(schema.Ref))
			return
		}
		if _, ok := seen[schema]; ok {
			return
		}
		seen[schema] = struct{}{}
		if ap, ok := schema.AdditionalProperties.(bool); ok && !ap {
			strict = append(strict, name)
		}
		for _, prop := range schema.Properties {
			walk(name, prop)
		}
		walk(name, schema.Items)
		if sub, ok := schema.AdditionalProperties.(*huma.Schema); ok {
			walk(name, sub)
		}
		for _, child := range schema.OneOf {
			walk(name, child)
		}
		for _, child := range schema.AnyOf {
			walk(name, child)
		}
		for _, child := range schema.AllOf {
			walk(name, child)
		}
		walk(name, schema.Not)
	}

	for _, item := range doc.Paths {
		if item == nil {
			continue
		}
		for _, op := range []*huma.Operation{
			item.Get, item.Put, item.Post, item.Delete,
			item.Options, item.Head, item.Patch, item.Trace,
		} {
			if op == nil {
				continue
			}
			for _, resp := range op.Responses {
				for _, mt := range resp.Content {
					walk("", mt.Schema)
				}
			}
		}
	}

	require.Empty(t, strict, "response-reachable schemas must permit unknown fields")
}

// TestSchemaSharedByRequestAndResponseStaysStrict ensures relaxing responses
// never loosens request validation. A schema reachable from both a request body
// and a response must keep additionalProperties:false so the daemon still
// rejects unknown request fields, while a response-only schema is still relaxed.
func TestSchemaSharedByRequestAndResponseStaysStrict(t *testing.T) {
	shared := &huma.Schema{Type: huma.TypeObject, AdditionalProperties: false}
	responseOnly := &huma.Schema{Type: huma.TypeObject, AdditionalProperties: false}
	doc := &huma.OpenAPI{
		Components: &huma.Components{
			Schemas: huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer),
		},
		Paths: map[string]*huma.PathItem{
			"/share": {Post: &huma.Operation{
				RequestBody: &huma.RequestBody{Content: map[string]*huma.MediaType{
					"application/json": {Schema: shared},
				}},
				Responses: map[string]*huma.Response{"200": {Content: map[string]*huma.MediaType{
					"application/json": {Schema: responseOnly},
				}}},
			}},
			"/echo": {Get: &huma.Operation{
				Responses: map[string]*huma.Response{"200": {Content: map[string]*huma.MediaType{
					"application/json": {Schema: shared},
				}}},
			}},
		},
	}

	relaxResponseAdditionalProperties(doc)

	require.Equal(t, false, shared.AdditionalProperties, "shared request/response schema must stay strict")
	require.Equal(t, true, responseOnly.AdditionalProperties, "response-only schema must be relaxed")
}

// TestClientDocumentLeavesResponseAdditionalPropertiesUnset pins the client
// generator flavor (kata openapi --version 3.0): response schemas leave
// additionalProperties unset. Absence carries the same permissive semantics
// as the published 3.1 document's explicit true, but the Go client generator
// (oapi-codegen-dd) models optional object-typed properties as value types
// whenever the target schema carries an explicit additionalProperties
// constraint. Request schemas stay strict in both flavors.
func TestClientDocumentLeavesResponseAdditionalPropertiesUnset(t *testing.T) {
	doc := openAPIClientDocument()
	schemas := doc.Components.Schemas.Map()

	linkChanges := schemas["LinkChanges"]
	require.NotNil(t, linkChanges, "missing LinkChanges schema")
	require.Nil(t, linkChanges.AdditionalProperties,
		"client-flavor response schemas must leave additionalProperties unset")

	request := schemas["CreateIssueRequestBody"]
	require.NotNil(t, request, "missing CreateIssueRequestBody schema")
	require.Equal(t, false, request.AdditionalProperties,
		"client-flavor request bodies must stay strict")
}

func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func assertSchemaPropertyType(t *testing.T, doc *huma.OpenAPI, schemaName, propertyName, want string) {
	t.Helper()
	schema := doc.Components.Schemas.Map()[schemaName]
	if schema == nil {
		t.Fatalf("missing schema %s", schemaName)
	}
	prop := schema.Properties[propertyName]
	if prop == nil {
		t.Fatalf("missing %s.%s schema property", schemaName, propertyName)
	}
	if prop.Type != want {
		t.Fatalf("%s.%s type = %q, want %q", schemaName, propertyName, prop.Type, want)
	}
}

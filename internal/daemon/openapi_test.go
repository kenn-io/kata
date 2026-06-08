package daemon

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/danielgtaylor/huma/v2"
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
	assertSchemaPropertyType(t, doc, "ProjectOut", "metadata", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "ReadyGlobalIssue", "metadata", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "Recurrence", "template_labels", huma.TypeArray)
	assertSchemaPropertyType(t, doc, "Recurrence", "template_metadata", huma.TypeObject)
	assertSchemaPropertyType(t, doc, "RecurrenceTemplateUpdateInput", "metadata", huma.TypeObject)

	labels := doc.Components.Schemas.Map()["Recurrence"].Properties["template_labels"]
	if labels.Items == nil || labels.Items.Type != huma.TypeString {
		t.Fatalf("Recurrence.template_labels items = %+v, want string items", labels.Items)
	}
	updateMetadata := doc.Components.Schemas.Map()["RecurrenceTemplateUpdateInput"].Properties["metadata"]
	if !updateMetadata.Nullable {
		t.Fatal("RecurrenceTemplateUpdateInput.metadata must allow null")
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

package daemon

import (
	"bytes"
	"os"
	"testing"
)

// artifactPath is the committed OpenAPI schema, relative to this package dir.
// A const (not a var) keeps gosec G304 quiet: the path is fixed, not caller-supplied.
const artifactPath = "../../api/openapi.yaml"

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
		t.Fatalf("%s is stale; run `make openapi` to regenerate", artifactPath)
	}
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

package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/hooks"
)

func TestEveryRegisteredOperationHasHostPolicy(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })

	registered := make(map[string]struct{})
	for _, pathItem := range srv.API().OpenAPI().Paths {
		operations := []*huma.Operation{
			pathItem.Get, pathItem.Put, pathItem.Post, pathItem.Delete,
			pathItem.Options, pathItem.Head, pathItem.Patch, pathItem.Trace,
		}
		for _, operation := range operations {
			if operation == nil {
				continue
			}
			policy, ok := hostOperationPolicy(operation.OperationID)
			if !ok {
				t.Errorf("operation %q has no host policy", operation.OperationID)
				continue
			}
			if policy.Kind == "" || policy.Capability == "" {
				t.Errorf("operation %q has incomplete host policy: %+v", operation.OperationID, policy)
			}
			registered[operation.OperationID] = struct{}{}
		}
	}
	registered["openAPI"] = struct{}{}

	var stale []string
	for operationID := range hostOperationPolicies {
		if _, ok := registered[operationID]; !ok {
			stale = append(stale, operationID)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("host policies reference unregistered operations: %v", stale)
	}
}

// TestServerConfig_NilHooks_FillsNoop verifies NewServer substitutes a
// hooks.NewNoop() Sink when ServerConfig.Hooks is nil so handler tests
// that don't wire a dispatcher can still trigger mutations safely.
func TestServerConfig_NilHooks_FillsNoop(t *testing.T) {
	cfg := ServerConfig{Hooks: nil}
	srv := NewServer(cfg)
	t.Cleanup(func() { _ = srv.Close() })
	if srv.cfg.Hooks == nil {
		t.Fatal("Hooks should default to NewNoop, not stay nil")
	}
	if _, ok := srv.cfg.Hooks.(*hooks.Dispatcher); ok {
		t.Fatal("default Hooks should be Noop, not Dispatcher")
	}
}

func TestServerServesOpenAPIYAML(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /openapi.yaml status = %d, want %d; body: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/openapi+yaml" {
		t.Fatalf("Content-Type = %q, want application/openapi+yaml", got)
	}
	want, err := OpenAPIYAML()
	if err != nil {
		t.Fatalf("OpenAPIYAML: %v", err)
	}
	if !bytes.Equal(rr.Body.Bytes(), want) {
		t.Fatal("GET /openapi.yaml did not return the daemon OpenAPI YAML")
	}
}

func TestServerDoesNotServeDefaultOpenAPIJSON(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })

	for _, path := range []string{"/openapi", "/openapi.json", "/docs"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))

		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want %d", path, rr.Code, http.StatusNotFound)
		}
	}
}

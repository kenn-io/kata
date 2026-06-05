package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kenn.io/kata/internal/hooks"
)

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

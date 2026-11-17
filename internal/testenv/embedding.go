package testenv

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// NewTwoDimensionalEmbeddingServer starts an embedding provider for service tests.
func NewTwoDimensionalEmbeddingServer(t *testing.T, observe func([]string)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if observe != nil {
			observe(request.Input)
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			data[i] = map[string]any{"embedding": []float32{1, 0}, "index": i}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

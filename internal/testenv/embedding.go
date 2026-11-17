package testenv

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// NewEmbeddingServer starts a test provider. embed maps each input to its
// vector; nil embeds every input as {1, 0}.
func NewEmbeddingServer(t *testing.T, embed func(string) []float32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		data := make([]map[string]any, len(request.Input))
		for i, input := range request.Input {
			vector := []float32{1, 0}
			if embed != nil {
				vector = embed(input)
			}
			data[i] = map[string]any{"embedding": vector, "index": i}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

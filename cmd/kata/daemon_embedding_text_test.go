package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/embedding"
)

func TestEmbeddingStartupForwardsLiteralTextControls(t *testing.T) {
	var inputs [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      []string `json:"input"`
			Dimensions *int     `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Dimensions == nil || *req.Dimensions != 768 {
			t.Errorf("dimensions = %v, want 768", req.Dimensions)
		}
		inputs = append(inputs, req.Input)
		v := make([]float32, 768)
		v[0] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": v}}})
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf(`
[search.embeddings]
base_url = %q
model = "example-model"
dims = 768
request_dimensions = true
document_prefix = "title: none | text: "
document_suffix = " document end "
query_prefix = "task: search result | query: "
query_suffix = " query end "
`, srv.URL)), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	c, _, err := preflightEmbeddingStartup(cfg.Search.Embeddings, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = c.EncodeFunc()(context.Background(), []string{embedding.EmbedText("login", "expired")})
	require.NoError(t, err)
	_, err = c.EmbedQuery(context.Background(), []string{"login"})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"title: none | text: login\n\nexpired document end "}, {"task: search result | query: login query end "}}, inputs)
}

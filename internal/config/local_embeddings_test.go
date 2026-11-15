package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestLocalEmbeddingsConfig(t *testing.T) {
	for _, key := range []string{"KATA_SEARCH_EMBEDDINGS_BASE_URL", "KATA_SEARCH_EMBEDDINGS_MODEL", "KATA_SEARCH_EMBEDDINGS_DIMS", "KATA_SEARCH_EMBEDDINGS_API_KEY_FILE", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_SUFFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_PREFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_SUFFIX", "KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS"} {
		t.Setenv(key, "")
	}
	const invalidLocal = "search.embeddings: local requires a nomic-embed-text model and dims 768, or request_dimensions for a smaller width"
	for _, tt := range []struct {
		name, text, wantError, wantModel, wantURL string
		wantLocal                                 bool
		env                                       map[string]string
	}{
		{name: "enabled", text: "local = true", wantModel: "nomic-embed-text", wantLocal: true},
		{name: "configured server", text: "local = true", wantModel: "mxbai-embed-large", wantURL: "http://127.0.0.1:11434/v1", env: map[string]string{"KATA_SEARCH_EMBEDDINGS_BASE_URL": "http://127.0.0.1:11434/v1", "KATA_SEARCH_EMBEDDINGS_MODEL": "mxbai-embed-large"}},
		{name: "wrong model", text: "local = true\nmodel = 'mxbai-embed-large'", wantError: invalidLocal},
		{name: "too wide", text: "local = true\ndims = 769", wantError: invalidLocal},
		{name: "smaller width requested", text: "local = true\ndims = 512\nrequest_dimensions = true", wantModel: "nomic-embed-text", wantLocal: true},
		{name: "smaller width omitted", text: "local = true\ndims = 512", wantError: invalidLocal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\n" + tt.text + "\n"))
			if tt.wantError != "" {
				require.EqualError(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			require.True(t, cfg.Embeddings.Enabled())
			require.Equal(t, tt.wantLocal, cfg.Embeddings.UsesLocal())
			require.Equal(t, tt.wantURL, cfg.Embeddings.BaseURL)
			require.Equal(t, tt.wantModel, cfg.Embeddings.Model)
		})
	}
}

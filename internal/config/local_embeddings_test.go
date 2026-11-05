package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestLocalEmbeddingsConfig(t *testing.T) {
	for _, tt := range []struct{ name, text, wantError string }{
		{"enabled", "local = true", ""},
		{"configured server", "local = true", ""},
		{"wrong model", "local = true\nmodel = 'mxbai-embed-large'", "model"},
		{"too wide", "local = true\ndims = 769", "dims"},
		{"smaller width requested", "local = true\ndims = 512\nrequest_dimensions = true", ""},
		{"smaller width omitted", "local = true\ndims = 512", "dims"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "configured server" {
				t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "http://127.0.0.1:11434/v1")
				t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "mxbai-embed-large")
			}
			cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\n" + tt.text + "\n"))
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			require.True(t, cfg.Embeddings.Enabled())
			if tt.name == "configured server" {
				require.False(t, cfg.Embeddings.UsesLocal())
				require.Equal(t, "http://127.0.0.1:11434/v1", cfg.Embeddings.BaseURL)
				require.Equal(t, "mxbai-embed-large", cfg.Embeddings.Model)
			}
			if tt.name == "enabled" {
				require.Equal(t, "nomic-embed-text", cfg.Embeddings.Model)
			}
		})
	}
}

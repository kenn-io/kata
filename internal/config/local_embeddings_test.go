package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestLocalEmbeddingsConfig(t *testing.T) {
	for _, tt := range []struct{ name, text, wantError string }{
		{"enabled", "local = true", ""},
		{"endpoint conflict", "local = true\nbase_url = 'http://127.0.0.1:11434/v1'", "base_url"},
		{"wrong model", "local = true\nmodel = 'mxbai-embed-large'", "model"},
		{"too wide", "local = true\ndims = 769", "dims"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\n" + tt.text + "\n"))
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			require.True(t, cfg.Embeddings.Enabled())
		})
	}
}

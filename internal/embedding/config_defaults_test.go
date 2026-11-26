package embedding

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestEmbeddingDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  config.EmbeddingsConfig
		batch   int
		timeout time.Duration
	}{
		{"built-in", config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text"}, 8, 5 * time.Minute},
		{"configured", config.EmbeddingsConfig{Local: true, BaseURL: "http://127.0.0.1:1234/v1", Model: "m"}, 64, 0},
		{"explicit", config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text", BatchSize: 17, TimeoutSeconds: 42}, 17, 42 * time.Second},
		{"batch override", config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text", BatchSize: 17}, 17, 5 * time.Minute},
		{"timeout override", config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text", TimeoutSeconds: 42}, 8, 42 * time.Second},
		{"vertex capped", config.EmbeddingsConfig{API: "vertex", BaseURL: "https://example.com/publishers/google/models", Model: " gemini-embedding-001 ", BatchSize: 9}, 1, 0},
		{"vertex other model", config.EmbeddingsConfig{API: "vertex", BaseURL: "https://example.com/publishers/google/models", Model: "other-model", BatchSize: 9}, 9, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ClientConfig(tc.config)
			if tc.config.UsesLocal() {
				cfg.Endpoint = func(context.Context) (string, error) { return "http://127.0.0.1:1234/v1", nil }
			}
			client, err := New(cfg)
			require.NoError(t, err)
			require.Equal(t, tc.batch, client.BatchSize())
			require.Equal(t, tc.timeout, cfg.Timeout)
		})
	}
}

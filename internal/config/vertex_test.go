package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

func TestVertexEmbeddingsConfig(t *testing.T) {
	var cfg DaemonConfig
	_, err := toml.Decode("[search.embeddings]\napi = 'vertex'\nbase_url = 'https://example.com/publishers/google/models'\nmodel = 'gemini-embedding-001'\n", &cfg)
	require.NoError(t, err)
	require.Equal(t, embedconfig.APIVertex, cfg.Search.Embeddings.API)
	require.NoError(t, ValidateEmbeddings(cfg.Search.Embeddings))
	for _, invalid := range []EmbeddingsConfig{
		{API: "unknown"}, {API: embedconfig.APIVertex},
	} {
		require.Error(t, ValidateEmbeddings(invalid))
	}
	for _, alter := range []func(*EmbeddingsConfig){
		func(e *EmbeddingsConfig) { e.Local = true },
		func(e *EmbeddingsConfig) { e.APIKey = "key" },
		func(e *EmbeddingsConfig) { e.APIKeyFile = "key" },
		func(e *EmbeddingsConfig) { e.APIKeyEnv = "KEY" },
		func(e *EmbeddingsConfig) { e.RequestDimensions = true },
	} {
		invalid := cfg.Search.Embeddings
		alter(&invalid)
		require.Error(t, ValidateEmbeddings(invalid))
	}
}

func TestVertexEmbeddingsEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SEARCH_EMBEDDINGS_API", " vertex ")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "https://example.com/publishers/google/models")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "gemini-embedding-001")
	cfg, err := ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t, embedconfig.APIVertex, cfg.Search.Embeddings.API)
	require.Equal(t, "KATA_SEARCH_EMBEDDINGS_API", cfg.Sources["search_embeddings_api"])
	t.Setenv("KATA_SEARCH_EMBEDDINGS_API", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[search.embeddings]\napi = ' vertex '\n"), 0o600))
	cfg, err = ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t, embedconfig.APIVertex, cfg.Search.Embeddings.API)
}

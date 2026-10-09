package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// Contract: plain environment settings replace packaging config seeding.
func TestDeploymentEnvironmentOverridesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`listen = "127.0.0.1:7000"
[web]
listen = "127.0.0.1:7001"
public_origin = "https://old.example"
[search.embeddings]
base_url = "https://old.example/v1"
model = "old-model"
dims = 128
`), 0o600))
	t.Setenv("KATA_LISTEN", " 127.0.0.1:7777 ")
	t.Setenv("KATA_WEB_LISTEN", "127.0.0.1:7778")
	t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "http://daemon.example:80")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "https://embedding.example/v1")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "example-model")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", "1024")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_API_KEY_FILE", "/run/secrets/embedding-key")
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:7777", cfg.Listen)
	require.Equal(t, "127.0.0.1:7778", cfg.Web.Listen)
	require.Equal(t, "http://daemon.example", cfg.Web.PublicOrigin)
	require.Equal(t, "https://embedding.example/v1", cfg.Search.Embeddings.BaseURL)
	require.Equal(t, "example-model", cfg.Search.Embeddings.Model)
	require.Equal(t, 1024, cfg.Search.Embeddings.Dims)
	require.Equal(t, "/run/secrets/embedding-key", cfg.Search.Embeddings.APIKeyFile)
	auth, err := config.ReadAuthConfig()
	require.NoError(t, err)
	require.True(t, auth.TrustPrivateNetwork)
}

func TestDeploymentInvalidEmbeddingDimensionsFailLoudly(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	for _, value := range []string{"wrong", "-1", "92233720368547758070"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", value)
			_, err := config.ReadDaemonConfig()
			require.Error(t, err)
			require.Contains(t, err.Error(), "DIMS")
		})
	}
}

// Contract: container env can carry literal role prompts, whose leading and
// trailing spaces are part of the text sent to the provider.
func TestDeploymentEmbeddingTextControlsOverrideFileVerbatim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[search.embeddings]
base_url = "https://embedding.example/v1"
model = "example-model"
document_prefix = "file document: "
document_suffix = " file end"
query_prefix = "file query: "
query_suffix = " file end"
request_dimensions = true
`), 0o600))
	t.Setenv("KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX", "title: none | text: ")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_DOCUMENT_SUFFIX", " document end ")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_QUERY_PREFIX", "task: search result | query: ")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_QUERY_SUFFIX", " query end ")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS", " false ")
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	ec := cfg.Search.Embeddings
	require.Equal(t, "title: none | text: ", ec.DocumentPrefix)
	require.Equal(t, " document end ", ec.DocumentSuffix)
	require.Equal(t, "task: search result | query: ", ec.QueryPrefix)
	require.Equal(t, " query end ", ec.QuerySuffix)
	require.False(t, ec.RequestDimensions)
}

func TestDeploymentInvalidRequestDimensionsFailLoudly(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS", "sometimes")
	_, err := config.ReadDaemonConfig()
	require.ErrorContains(t, err, "KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS")
}

func TestLocalProfileRejectsDeploymentControlReferences(t *testing.T) {
	for _, key := range []string{"KATA_LISTEN", "KATA_WEB_LISTEN", "KATA_WEB_PUBLIC_ORIGIN", "KATA_AUTH_TOKEN_FILE", "KATA_SEARCH_EMBEDDINGS_BASE_URL", "KATA_SEARCH_EMBEDDINGS_MODEL", "KATA_SEARCH_EMBEDDINGS_DIMS", "KATA_SEARCH_EMBEDDINGS_API_KEY_FILE", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_SUFFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_PREFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_SUFFIX", "KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS", "KATA_BACKUP_DIR", "KATA_BACKUP_INTERVAL", "KATA_BACKUP_RETAIN"} {
		t.Run(key, func(t *testing.T) {
			_, err := config.LocalProfileEnvironment(config.LocalProfileConfig{Home: t.TempDir(), Catalog: config.CatalogDaemonConfig{TokenEnv: key}}, true)
			require.ErrorContains(t, err, "reserved environment key")
		})
	}
}

// Empty Compose substitutions must preserve the operator's TOML settings.
func TestDeploymentEmptyEnvironmentPreservesFile(t *testing.T) {
	for _, value := range []string{"", " \t "} {
		t.Run(value, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`listen = "127.0.0.1:7000"
[web]
listen = "127.0.0.1:7001"
public_origin = "https://daemon.example"
[search.embeddings]
base_url = "https://embedding.example/v1"
model = "example-model"
dims = 128
api_key_file = "/run/secrets/embedding-key"
document_prefix = "file document: "
query_prefix = "file query: "
request_dimensions = true
[backup]
dir = "/data/backups"
interval = "24h"
retain = "720h"
`), 0o600))
			for _, name := range []string{"KATA_LISTEN", "KATA_WEB_LISTEN", "KATA_WEB_PUBLIC_ORIGIN", "KATA_SEARCH_EMBEDDINGS_BASE_URL", "KATA_SEARCH_EMBEDDINGS_MODEL", "KATA_SEARCH_EMBEDDINGS_API_KEY_FILE", "KATA_SEARCH_EMBEDDINGS_DIMS", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_SUFFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_PREFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_SUFFIX", "KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS", "KATA_BACKUP_DIR", "KATA_BACKUP_INTERVAL", "KATA_BACKUP_RETAIN"} {
				t.Setenv(name, value)
			}
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err)
			require.Equal(t, "127.0.0.1:7000", cfg.Listen)
			require.Equal(t, "127.0.0.1:7001", cfg.Web.Listen)
			require.Equal(t, "https://daemon.example", cfg.Web.PublicOrigin)
			require.Equal(t, "https://embedding.example/v1", cfg.Search.Embeddings.BaseURL)
			require.Equal(t, "example-model", cfg.Search.Embeddings.Model)
			require.Equal(t, 128, cfg.Search.Embeddings.Dims)
			require.Equal(t, "/run/secrets/embedding-key", cfg.Search.Embeddings.APIKeyFile)
			require.Equal(t, "file document: ", cfg.Search.Embeddings.DocumentPrefix)
			require.Equal(t, "file query: ", cfg.Search.Embeddings.QueryPrefix)
			require.True(t, cfg.Search.Embeddings.RequestDimensions)
			require.Equal(t, config.BackupConfig{Dir: "/data/backups", Interval: "24h", Retain: "720h"}, cfg.Backup)
		})
	}
}

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/embedlocal"
)

func stubLocalEmbeddingsInstall(t *testing.T) {
	t.Helper()
	original := localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsInstall = original })
	localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
}

func TestLocalEmbeddingsTOMLLayouts(t *testing.T) {
	for _, key := range []string{"KATA_SEARCH_EMBEDDINGS_BASE_URL", "KATA_SEARCH_EMBEDDINGS_MODEL", "KATA_SEARCH_EMBEDDINGS_DIMS", "KATA_SEARCH_EMBEDDINGS_API_KEY_FILE", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_SUFFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_PREFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_SUFFIX", "KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS"} {
		t.Setenv(key, "")
	}
	prefix := "title   = \"\"\"a fake header\n[search.embeddings]\n\"\"\"\nports = [\n  1,\n  2,\n]\n# before search\n"
	suffix := "[display]\n# unrelated comment\n"
	for _, tc := range []struct{ name, body string }{
		{name: "incompatible model", body: "[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='mxbai-embed-large'\ndims=1024\n"},
		{name: "local salt", body: "[search.embeddings]\nlocal=true\nfingerprint_salt='manual-reindex'\n"},
		{name: "BOM", body: "\ufeff[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n"},
		{name: "credentials", body: "[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1' # endpoint comment\nmodel='nomic-embed-text'\napi_key='example-key'\napi_key_file='/example/embedding.key'\napi_key_env='EXAMPLE_EMBEDDING_KEY'\n"},
		{name: "spaced table", body: "[search . embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n"},
		{name: "quoted table", body: "[search.\"embeddings\"]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n"},
		{name: "inline table", body: "[search]\nembeddings={base_url='http://127.0.0.1:11434/v1',model='nomic-embed-text'}\n"},
		{name: "multiline inline table", body: "[search]\nembeddings={\n # nested comment\n base_url='http://127.0.0.1:11434/v1',\n model='nomic-embed-text',\n}\n"},
		{name: "dotted embeddings", body: "[search]\nembeddings.base_url='http://127.0.0.1:11434/v1'\nembeddings.model='nomic-embed-text'\n"},
		{name: "dotted search", body: "search.embeddings.base_url='http://127.0.0.1:11434/v1'\nsearch.embeddings.model='nomic-embed-text'\n"},
		{name: "inline search", body: "search={embeddings={base_url='http://127.0.0.1:11434/v1',model='nomic-embed-text'}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			text := prefix + strings.TrimPrefix(body, "\ufeff") + suffix
			if strings.HasPrefix(body, "\ufeff") {
				text = "\ufeff" + text
			}
			current, err := config.ParseSearchConfig([]byte(text))
			require.NoError(t, err)
			result, err := localEmbeddingsConfig([]byte(text), current.Embeddings, "")
			require.NoError(t, err)
			next, err := config.ParseSearchConfig(result)
			require.NoError(t, err)
			require.True(t, next.Embeddings.Local)
			require.Empty(t, next.Embeddings.BaseURL)
			require.Equal(t, current.Embeddings.FingerprintSalt, next.Embeddings.FingerprintSalt)
			require.Equal(t, current.Embeddings.APIKey, next.Embeddings.APIKey)
			require.Equal(t, current.Embeddings.APIKeyFile, next.Embeddings.APIKeyFile)
			require.Equal(t, current.Embeddings.APIKeyEnv, next.Embeddings.APIKeyEnv)
			require.Equal(t, strings.HasPrefix(text, "\ufeff"), strings.HasPrefix(string(result), "\ufeff"))
			again, err := localEmbeddingsConfig(result, next.Embeddings, "")
			require.NoError(t, err)
			require.Equal(t, result, again, "retry must preserve the already-written configuration")
			require.Contains(t, string(result), prefix)
			require.Contains(t, string(result), suffix)
			if strings.Contains(body, "base_url") {
				require.Contains(t, string(result), "# base_url = ")
			}
			if strings.Contains(body, "mxbai-embed-large") {
				require.Contains(t, string(result), "# model = \"mxbai-embed-large\"")
				require.Contains(t, string(result), "# dims = 1024")
			}
			if strings.Contains(body, "# nested comment") {
				require.Contains(t, string(result), "# nested comment")
			}
			if strings.Contains(body, "# endpoint comment") {
				require.Contains(t, string(result), "# endpoint comment")
			}
		})
	}
}

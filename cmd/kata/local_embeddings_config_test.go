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

func clearLocalEmbeddingsEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"KATA_SEARCH_EMBEDDINGS_BASE_URL", "KATA_SEARCH_EMBEDDINGS_MODEL", "KATA_SEARCH_EMBEDDINGS_DIMS", "KATA_SEARCH_EMBEDDINGS_API_KEY_FILE", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_SUFFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_PREFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_SUFFIX", "KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS"} {
		t.Setenv(key, "")
	}
	return home
}

func TestLocalEmbeddingsTOMLLayouts(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	prefix := "title   = \"\"\"a fake header\n[search.embeddings]\n\"\"\"\nports = [\n  1,\n  2,\n]\n# before search\n"
	suffix := "[display]\n# unrelated comment\n"
	for _, tc := range []struct {
		name, body, modelEnv, dimsEnv, wantSalt string
		comments                                []string
		wantDims                                int
	}{
		{name: "external revision", body: "[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\ndims=768\nfingerprint_salt='changed-nomic-weights'\n", comments: []string{"# fingerprint_salt = \"changed-nomic-weights\""}, wantDims: 768},
		{name: "temporary recipe override", body: "[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='mxbai-embed-large'\ndims=1024\n", modelEnv: "nomic-embed-text", dimsEnv: "768", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\"", "# model = \"mxbai-embed-large\"", "# dims = 1024"}, wantDims: 768},
		{name: "local salt", body: "[search.embeddings]\nlocal=true\nfingerprint_salt='manual-reindex'\n", wantSalt: "manual-reindex"},
		{name: "BOM", body: "\ufeff[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\""}},
		{name: "credentials", body: "[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1' # endpoint comment\nmodel='nomic-embed-text'\napi_key='example-key'\napi_key_file='/example/embedding.key'\napi_key_env='EXAMPLE_EMBEDDING_KEY'\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\"", "# endpoint comment"}},
		{name: "spaced table", body: "[search . embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\""}},
		{name: "quoted table", body: "[search.\"embeddings\"]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\""}},
		{name: "inline table", body: "[search]\nembeddings={base_url='http://127.0.0.1:11434/v1',model='nomic-embed-text'}\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\""}},
		{name: "multiline inline table", body: "[search]\nembeddings={\n # nested comment\n base_url='http://127.0.0.1:11434/v1',\n model='nomic-embed-text',\n}\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\"", "# nested comment"}},
		{name: "dotted embeddings", body: "[search]\nembeddings.base_url='http://127.0.0.1:11434/v1'\nembeddings.model='nomic-embed-text'\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\""}},
		{name: "dotted search", body: "search.embeddings.base_url='http://127.0.0.1:11434/v1'\nsearch.embeddings.model='nomic-embed-text'\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\""}},
		{name: "inline search", body: "search={embeddings={base_url='http://127.0.0.1:11434/v1',model='nomic-embed-text'}}\n", comments: []string{"# base_url = \"http://127.0.0.1:11434/v1\""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", tc.modelEnv)
			t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", tc.dimsEnv)
			body := tc.body
			text := prefix + strings.TrimPrefix(body, "\ufeff") + suffix
			if strings.HasPrefix(body, "\ufeff") {
				text = "\ufeff" + text
			}
			current, err := config.ParseSearchConfig([]byte(text))
			require.NoError(t, err)
			result, err := localEmbeddingsConfig([]byte(text), current.Embeddings, "")
			require.NoError(t, err)
			next, err := config.ParseSearchFile(result)
			require.NoError(t, err)
			require.True(t, next.Embeddings.Local)
			require.Empty(t, next.Embeddings.BaseURL)
			require.Equal(t, tc.wantSalt, next.Embeddings.FingerprintSalt)
			require.Equal(t, "nomic-embed-text", next.Embeddings.Model)
			require.Equal(t, tc.wantDims, next.Embeddings.Dims)
			require.Equal(t, current.Embeddings.APIKey, next.Embeddings.APIKey)
			require.Equal(t, current.Embeddings.APIKeyFile, next.Embeddings.APIKeyFile)
			require.Equal(t, current.Embeddings.APIKeyEnv, next.Embeddings.APIKeyEnv)
			require.Equal(t, strings.HasPrefix(text, "\ufeff"), strings.HasPrefix(string(result), "\ufeff"))
			again, err := localEmbeddingsConfig(result, next.Embeddings, "")
			require.NoError(t, err)
			require.Equal(t, result, again, "retry must preserve the already-written configuration")
			require.Contains(t, string(result), prefix)
			require.Contains(t, string(result), suffix)
			for _, comment := range tc.comments {
				require.Contains(t, string(result), comment)
			}
		})
	}
}

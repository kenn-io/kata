package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
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

func TestLocalEmbeddingsTemporaryRecipeOverride(t *testing.T) {
	home := setupKataEnv(t)
	stubLocalEmbeddingsInstall(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "nomic-embed-text")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", "768")
	oldEndpoint := localEmbeddingsEndpoint
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
	path := filepath.Join(home, "config.toml")
	original := "[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='mxbai-embed-large'\ndims=1024\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err, out+stderr)
	t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", "")
	next, err := config.ReadSearchConfig()
	require.NoError(t, err, "persisted setup must remain valid after temporary overrides disappear")
	require.True(t, next.Embeddings.UsesLocal())
}

func TestLocalEmbeddingsRestartRefusal(t *testing.T) {
	home := setupKataEnv(t)
	stubLocalEmbeddingsInstall(t)
	t.Setenv("KATA_SERVER", "")
	oldEndpoint := localEmbeddingsEndpoint
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:12345"})
	require.NoError(t, err)
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.ErrorContains(t, err, "older kata release")
	_, err = os.Stat(filepath.Join(home, "config.toml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalEmbeddingsTOMLLayouts(t *testing.T) {
	prefix := "title   = \"\"\"a fake header\n[search.embeddings]\n\"\"\"\nports = [\n  1,\n  2,\n]\n# before search\n"
	suffix := "[display]\n# unrelated comment\n"
	for _, body := range []string{
		"[search.embeddings]\nlocal=true\nfingerprint_salt='manual-reindex'\n",
		"\ufeff[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n",
		"[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1' # endpoint comment\nmodel='nomic-embed-text'\n",
		"[search . embeddings]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n",
		"[search.\"embeddings\"]\nbase_url='http://127.0.0.1:11434/v1'\nmodel='nomic-embed-text'\n",
		"[search]\nembeddings={base_url='http://127.0.0.1:11434/v1',model='nomic-embed-text'}\n",
		"[search]\nembeddings={\n # nested comment\n base_url='http://127.0.0.1:11434/v1',\n model='nomic-embed-text',\n}\n",
		"[search]\nembeddings.base_url='http://127.0.0.1:11434/v1'\nembeddings.model='nomic-embed-text'\n",
		"search.embeddings.base_url='http://127.0.0.1:11434/v1'\nsearch.embeddings.model='nomic-embed-text'\n",
		"search={embeddings={base_url='http://127.0.0.1:11434/v1',model='nomic-embed-text'}}\n",
	} {
		t.Run(body, func(t *testing.T) {
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
			require.Equal(t, strings.HasPrefix(text, "\ufeff"), strings.HasPrefix(string(result), "\ufeff"))
			again, err := localEmbeddingsConfig(result, next.Embeddings, "")
			require.NoError(t, err)
			require.Equal(t, result, again, "retry must preserve the already-written configuration")
			require.Contains(t, string(result), prefix)
			require.Contains(t, string(result), suffix)
			if strings.Contains(body, "# nested comment") {
				require.Contains(t, string(result), "# nested comment")
			}
			if strings.Contains(body, "# endpoint comment") {
				require.Contains(t, string(result), "# endpoint comment")
			}
		})
	}
}

func TestLocalEmbeddingsReducedWidth(t *testing.T) {
	home := setupKataEnv(t)
	stubLocalEmbeddingsInstall(t)
	t.Setenv("KATA_SERVER", "")
	oldEndpoint := localEmbeddingsEndpoint
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
	for _, requestsWidth := range []bool{false, true} {
		text := fmt.Sprintf("[search.embeddings]\nbase_url = 'http://127.0.0.1:11434/v1'\nmodel = 'nomic-embed-text'\ndims = 512\nrequest_dimensions = %t\n", requestsWidth)
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(text), 0o600))
		_, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
		require.NoError(t, err)
		index := "rebuilt"
		if requestsWidth {
			index = "kept"
		}
		require.Contains(t, stderr, "index will be "+index)
		next, err := config.ReadSearchConfig()
		require.NoError(t, err)
		width := 768
		if requestsWidth {
			width = 512
		}
		require.Equal(t, width, next.Embeddings.Dims)
	}
}

package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestLocalEmbeddingsRestartRefusal(t *testing.T) {
	home := setupKataEnv(t)
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
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.Error(t, err, "retry must still report the old daemon's failed activation")
	_, err = os.Stat(filepath.Join(home, "config.toml"))
	require.ErrorIs(t, err, os.ErrNotExist)
	// A later replacement failure leaves the setting persisted, but reruns must retry activation.
	oldExecutable := resolveDaemonExecutable
	t.Cleanup(func() { resolveDaemonExecutable = oldExecutable })
	resolveDaemonExecutable = func() (string, error) { return "", errors.New("unavailable executable") }
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:12345", Metadata: map[string]string{daemonruntime.RuntimeRestartExecutableKey: "1"}})
	require.NoError(t, err)
	for range 2 {
		out, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
		require.ErrorContains(t, err, "cannot resolve the installed restart executable")
		require.Empty(t, out)
	}
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.Local)
}

func TestLocalEmbeddingsTOMLLayouts(t *testing.T) {
	prefix := "title   = \"\"\"a fake header\n[search.embeddings]\n\"\"\"\nports = [\n  1,\n  2,\n]\n# before search\n"
	suffix := "[display]\n# unrelated comment\n"
	for _, body := range []string{
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
			text := prefix + body + suffix
			current, err := config.ParseSearchConfig([]byte(text))
			require.NoError(t, err)
			result, err := localEmbeddingsConfig([]byte(text), current.Embeddings)
			require.NoError(t, err)
			next, err := config.ParseSearchConfig(result)
			require.NoError(t, err)
			require.True(t, next.Embeddings.Local)
			require.Empty(t, next.Embeddings.BaseURL)
			again, err := localEmbeddingsConfig(result, next.Embeddings)
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
	t.Setenv("KATA_SERVER", "")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dimensions int `json:"dimensions"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &req))
		dims := req.Dimensions
		// Kit embedserver defaults an omitted dimensions field to 768.
		if dims == 0 {
			dims = 768
		}
		vector := make([]float32, dims)
		vector[0] = 1
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"embedding": vector}}}))
	}))
	t.Cleanup(provider.Close)
	oldEndpoint := localEmbeddingsEndpoint
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return provider.URL, nil }
	for _, requestsWidth := range []bool{false, true} {
		current := config.EmbeddingsConfig{BaseURL: provider.URL, Model: "nomic-embed-text", Dims: 512, RequestDimensions: requestsWidth}
		require.Equal(t, requestsWidth, current.LocalRecipeCompatible())
		text := fmt.Sprintf("[search.embeddings]\nbase_url = %q\nmodel = 'nomic-embed-text'\ndims = 512\nrequest_dimensions = %t\n", provider.URL, requestsWidth)
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
		client, _, err := preflightEmbeddingStartup(next.Embeddings, "")
		require.NoError(t, err)
		vectors, err := client.Embed(t.Context(), []string{"hello"})
		require.NoError(t, err)
		require.Len(t, vectors[0], width)
	}
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalEmbeddingsSetupStatus(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	source := []byte("[search.embeddings]\nbase_url = \"http://127.0.0.1:11434/v1\"\nmodel = \"mxbai-embed-large\"\ndims = 1024\n")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), source, 0600))
	cfg, err := config.ParseSearchConfig(source)
	require.NoError(t, err)
	s := &localEmbeddingsSetup{embeddings: cfg.Embeddings}
	status, err := s.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ollama", status.Provider)
	require.False(t, status.CanChange)
	require.Nil(t, status.Job)
}

func TestLocalEmbeddingsSetupSavesUntilManualRestart(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	installed := false
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
		if !installed {
			return "", embedlocal.ErrNotFound
		}
		return "http://127.0.0.1:12345/v1", nil
	}
	localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		installed = true
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
	baseURL := startDaemonWithFederationConfig(t, "")
	client := &http.Client{Timeout: time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/api/v1/embeddings/local", strings.NewReader("{}"))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	require.NoError(t, response.Body.Close())
	var status api.LocalEmbeddingsStatus
	require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: waits for the daemon worker to save the configuration
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/v1/embeddings/local", nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		require.NoError(t, json.NewDecoder(response.Body).Decode(&status))
		return status.Job != nil && status.Job.State == "restart"
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "none", status.Provider, "the running daemon keeps its startup configuration")
	require.False(t, status.CanChange)
	path, err := config.DaemonConfigPath()
	require.NoError(t, err)
	data, err := os.ReadFile(path) // #nosec G304 -- configuration in the isolated test home
	require.NoError(t, err)
	require.Contains(t, string(data), "local = true")
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/v1/health", nil)
	require.NoError(t, err)
	response, err = client.Do(request)
	require.NoError(t, err, "setup leaves the daemon running")
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

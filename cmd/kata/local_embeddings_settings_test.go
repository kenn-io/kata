package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalEmbeddingsSetupStatus(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".kenn"), []byte("unrelated file"), 0o600))
	for _, tc := range []struct{ provider, url string }{
		{"ollama", "http://127.0.0.1:11434/v1"},
		{"other", "https://embeddings.example/v1"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			cfg, err := config.ParseSearchConfig(fmt.Appendf(nil, "[search.embeddings]\nbase_url = %q\nmodel = \"nomic-embed-text\"\ndims = 768\n", tc.url))
			require.NoError(t, err)
			setup := &localEmbeddingsSetup{embeddings: cfg.Embeddings}
			status, err := setup.Status(t.Context())
			require.NoError(t, err, "configured external search does not use the shared local installation")
			require.Equal(t, tc.provider, status.Provider)
			require.False(t, status.CanChange)
		})
	}
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
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/api/v1/embeddings/local", strings.NewReader("{}"))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusConflict, response.StatusCode, "the same owner remains authorized while setup is busy")
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

func TestLocalEmbeddingsTurnOnPreservesProviderSavedAfterStartup(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	original := []byte("[search.embeddings]\nbase_url = \"http://127.0.0.1:11434/v1\"\nmodel = \"nomic-embed-text\"\ndims = 768\n")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	var run func(context.Context)
	setup := &localEmbeddingsSetup{start: func(work func(context.Context)) bool { run = work; return true }}
	require.NoError(t, setup.TurnOn(t.Context()))
	require.ErrorIs(t, setup.TurnOn(t.Context()), daemon.ErrLocalEmbeddingsBusy)
	run(t.Context())
	require.Equal(t, "restart", setup.job.State)
	got, err := os.ReadFile(path) // #nosec G304 -- configuration in the isolated test home
	require.NoError(t, err)
	require.Equal(t, original, got, "a page loaded while search was off cannot switch a provider saved later")
}

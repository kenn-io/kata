package daemon_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

func TestEmbeddingProjectPolicySearchEndpoint(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer provider.Close()
	emb, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	require.NoError(t, err)
	idx, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, idx.Close()) })
	key := emb.Generation().Fingerprint()
	require.NoError(t, idx.EnsureBuilding(t.Context(), key, emb.Generation()))
	require.NoError(t, idx.CutOver(t.Context(), key))
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Embedder = emb
		cfg.VectorIndex = idx
		cfg.EmbeddingProjectAllowed = func(context.Context, string) (bool, error) { return false, nil }
	})
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "example task", Author: "tester"})
	require.NoError(t, err)
	for _, mode := range []string{"semantic", "hybrid"} {
		resp, body := envGetRaw(t, env, projectPath(project.ID)+"/search?q=example&mode="+mode)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	}
	resp, body := envGetRaw(t, env, projectPath(project.ID)+"/search?q=example&mode=auto")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), "example task")
	require.Contains(t, string(body), `"degraded":true`)
	require.Zero(t, requests.Load())
}

func TestEmbeddingProjectPolicyErrorSearchEndpoint(t *testing.T) {
	emb, err := embedding.New(embedding.Config{BaseURL: "https://embedder.example", Model: "m", Dims: 2})
	require.NoError(t, err)
	idx, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, idx.Close()) })
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Embedder = emb
		cfg.VectorIndex = idx
		cfg.EmbeddingProjectAllowed = func(context.Context, string) (bool, error) { return false, errors.New("internal policy detail") }
	})
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	resp, body := envGetRaw(t, env, projectPath(project.ID)+"/search?q=example&mode=semantic")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	require.Contains(t, string(body), "semantic search is unavailable for this project")
	require.NotContains(t, string(body), "internal policy detail")
}

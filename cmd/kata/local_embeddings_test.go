package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalEmbeddingsSetup(t *testing.T) {
	home := filepath.Join(setupKataEnv(t), "fresh")
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	t.Setenv("KATA_SERVER", "")
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	installed := false
	downloads := 0
	localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		require.True(t, opts.SkipOllama)
		if !installed {
			return "", embedlocal.ErrNotFound
		}
		return "http://127.0.0.1:12345/v1", nil
	}
	localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		expectedDir, err := embedlocal.DefaultDir()
		require.NoError(t, err)
		require.Equal(t, expectedDir, opts.DataDir)
		yes, err := opts.Confirm(ctx, 290_000_000)
		require.NoError(t, err)
		require.True(t, yes)
		downloads++
		installed = true
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
	out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err, stderr)
	require.Contains(t, stderr, "290")
	require.Contains(t, out, "kata health --json")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.Local)
	require.True(t, cfg.Embeddings.Enabled())
	require.FileExists(t, filepath.Join(home, "config.toml"))
	out, _, err = executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "index kept")
	require.Equal(t, 1, downloads)
}

func TestLocalEmbeddingsSetupRefusesEnvironmentOverride(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "http://127.0.0.1:11434/v1")
	original := []byte("# untouched\n")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, original, 0600))
	_, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.ErrorContains(t, err, "environment overrides")
	require.ErrorContains(t, err, "base_url")
	got, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestLocalEmbeddingsDaemonRefusesDownload(t *testing.T) {
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		yes, err := opts.Confirm(ctx, 290_000_000)
		require.False(t, yes)
		return embedlocal.Installation{}, err
	}
	client, _, err := preflightEmbeddingStartup(config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text"}, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), []string{"hello"})
	require.ErrorContains(t, err, "kata daemon local-embeddings")
}

func TestLocalEmbeddingsRecipeOffline(t *testing.T) {
	home := setupKataEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[search.embeddings]\nlocal = true\n"), 0600))
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
		t.Fatal("recipe contacted server")
		return "", nil
	}
	localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		t.Fatal("recipe installed server")
		return embedlocal.Installation{}, nil
	}
	out, stderr, err := executeRootCapture(t, t.Context(), "federation", "embedding-recipe")
	require.NoError(t, err, stderr)
	require.Contains(t, out, `"model":"nomic-embed-text"`)
}

func TestLocalEmbeddingsSwitchKeepsIndex(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "login crash", Author: "agent"})
	require.NoError(t, err)
	var oldCalls, newCalls atomic.Int64
	handler := func(counter *atomic.Int64) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Input      []string `json:"input"`
				Model      string   `json:"model"`
				Dimensions int      `json:"dimensions"`
			}
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			require.Equal(t, "nomic-embed-text", request.Model)
			counter.Add(int64(len(request.Input)))
			data := make([]map[string]any, len(request.Input))
			width := request.Dimensions
			if width == 0 {
				width = 768
			}
			for i := range data {
				vector := make([]float32, width)
				vector[0] = 1
				data[i] = map[string]any{"index": i, "embedding": vector}
			}
			require.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
		})
	}
	oldProvider := httptest.NewServer(handler(&oldCalls))
	t.Cleanup(oldProvider.Close)
	newProvider := httptest.NewServer(handler(&newCalls))
	t.Cleanup(newProvider.Close)
	original := fmt.Sprintf("# shared config\n[search.embeddings]\n# keep recipe\nbase_url = %q\nmodel = 'nomic-embed-text'\ndims = 768\ndocument_prefix = 'search_document: '\nquery_prefix = 'search_query: '\n[display]\n# untouched\n", oldProvider.URL)
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(original), 0640)) // #nosec G306 -- fixture exercises preservation of an existing mode
	oldEndpoint := localEmbeddingsEndpoint
	localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		require.True(t, opts.SkipOllama)
		return newProvider.URL, nil
	}
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	var firstFingerprint string
	var firstRecipe embedding.ArtifactIdentity
	for phase := range 2 {
		ec, err := config.ReadSearchConfig()
		require.NoError(t, err)
		client, _, err := preflightEmbeddingStartup(ec.Embeddings, filepath.Join(home, "kata.db"))
		require.NoError(t, err)
		recipe, err := client.ArtifactIdentity("", "", "")
		require.NoError(t, err)
		if phase == 0 {
			firstFingerprint = client.Generation().Fingerprint()
			firstRecipe = recipe
		} else {
			require.Equal(t, firstFingerprint, client.Generation().Fingerprint())
			require.Equal(t, firstRecipe, recipe)
		}
		ctx, cancel := context.WithCancel(t.Context())
		workers := newDaemonWorkerGroup()
		cycles := make(chan struct{}, 8)
		admission := func() (*activity.Lease, bool, <-chan struct{}) {
			return activity.NewLease(func() { cycles <- struct{}{} }, nil), true, nil
		}
		_, idx, health, wake, err := startEmbeddingReconciler(ctx, workers, admission, ec.Embeddings, client, filepath.Join(home, "vectors.db"), store, daemon.NewEventBroadcaster(), log.New(io.Discard, "", 0))
		require.NoError(t, err)
		stop := func() {
			cancel()
			require.True(t, workers.Wait(context.WithoutCancel(t.Context())))
			require.NoError(t, idx.Close())
		}
		t.Cleanup(func() { cancel(); workers.Wait(context.WithoutCancel(t.Context())) })
		select {
		case <-cycles:
		case <-time.After(10 * time.Second):
			t.Fatal("backfill did not finish")
		}
		require.Zero(t, health().Backlog)
		if phase == 0 {
			require.EqualValues(t, 1, oldCalls.Load())
			stop()
			oldProvider.Close()
			out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			require.NoError(t, err, out+stderr)
			require.Contains(t, stderr, "index will be kept")
			rewritten, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
			require.NoError(t, err)
			require.Contains(t, string(rewritten), "# keep recipe")
			require.Contains(t, string(rewritten), "[display]\n# untouched\n")
			require.NotContains(t, string(rewritten), "base_url")
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0640), info.Mode().Perm())
		} else {
			require.Zero(t, newCalls.Load(), "old issues must reuse stored vectors")
			_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "login timeout", Author: "agent"})
			require.NoError(t, err)
			wake()
			//nolint:kennlint // sleeptest: observe persisted indexing progress after the worker wake.
			require.Eventually(t, func() bool { return health().Embedded == 2 && health().Backlog == 0 }, 10*time.Second, 10*time.Millisecond)
			require.EqualValues(t, 1, newCalls.Load())
			server := daemon.NewServer(daemon.ServerConfig{DB: store, VectorIndex: idx, Embedder: client})
			httpServer := httptest.NewServer(server.Handler())
			t.Setenv("KATA_SERVER", httpServer.URL)
			out, stderr, err := executeRootCapture(t, t.Context(), "search", "login", "--project", "spoke-project", "--json")
			httpServer.Close()
			require.NoError(t, server.Close())
			require.NoError(t, err, stderr)
			require.Contains(t, out, `"mode":"hybrid"`)
			require.Contains(t, out, "login crash")
			require.Contains(t, out, "login timeout")
			stop()
		}
	}
}

func TestLocalEmbeddingsRebuildRefusal(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	path := filepath.Join(home, "config.toml")
	original := []byte("[search.embeddings]\nbase_url = 'http://127.0.0.1:11434/v1'\nmodel = 'mxbai-embed-large'\n")
	require.NoError(t, os.WriteFile(path, original, 0600))
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		approved, err := opts.Confirm(ctx, 290_000_000)
		require.False(t, approved)
		return embedlocal.Installation{}, err
	}
	_, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings")
	require.ErrorContains(t, err, "no TTY")
	require.Contains(t, stderr, "index will be rebuilt")
	require.Contains(t, stderr, "replicas need the same recipe")
	got, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
	require.NoError(t, err)
	require.Equal(t, original, got)
}

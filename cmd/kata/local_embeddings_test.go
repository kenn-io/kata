package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	kitdaemon "go.kenn.io/kit/daemon"
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
		require.True(t, opts.SkipOllama || opts.Executable == "")
		if !installed {
			return "", embedlocal.ErrNotFound
		}
		return "http://127.0.0.1:12345/v1", nil
	}
	localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		expectedDir, err := embedlocal.DefaultDir()
		require.NoError(t, err)
		require.Equal(t, expectedDir, opts.DataDir)
		if installed {
			return embedlocal.Installation{Executable: "/example/kennembed"}, nil
		}
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
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
		t.Fatal("environment refusal must precede server discovery")
		return "", nil
	}
	localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		t.Fatal("environment refusal must precede installation")
		return embedlocal.Installation{}, nil
	}
	original := []byte("# untouched\n")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, original, 0600))
	_, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.ErrorContains(t, err, "KATA_SEARCH_EMBEDDINGS_BASE_URL")
	got, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestLocalEmbeddingsFreshOllamaThenBuiltIn(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	installs := 0
	localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		installs++
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
	localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		if !opts.SkipOllama {
			return "http://127.0.0.1:11434/v1", nil
		}
		if opts.Executable == "" {
			return "", embedlocal.ErrNotFound
		}
		return "http://127.0.0.1:12345/v1", nil
	}
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err)
	require.Zero(t, installs, "a fresh home must use the ready Ollama without installation")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:11434/v1", cfg.Embeddings.BaseURL)
	require.Equal(t, "nomic-embed-text", cfg.Embeddings.Model)
	require.False(t, cfg.Embeddings.Local)
	require.Contains(t, out, "Ollama")
	_, _, err = executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err)
	require.Equal(t, 1, installs)
	cfg, err = config.ReadSearchConfig()
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.UsesLocal())
	require.FileExists(t, filepath.Join(home, "config.toml"))
}

func TestLocalEmbeddingsSetupRefusesChangedConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		original string
		exists   bool
		edited   string
		remove   bool
	}{
		{name: "edited", exists: true, original: "[daemon]\nport = 8888\n", edited: "[daemon]\nport = 9999\n"},
		{name: "created", edited: "[daemon]\nport = 9999\n"},
		{name: "created empty"},
		{name: "removed", exists: true, original: "[daemon]\nport = 8888\n", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_SERVER", "")
			path := filepath.Join(home, "config.toml")
			if tc.exists {
				require.NoError(t, os.WriteFile(path, []byte(tc.original), 0600))
			}
			oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
			t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
			installed := false
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
				if !installed {
					return "", embedlocal.ErrNotFound
				}
				return "http://127.0.0.1:12345/v1", nil
			}
			localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				yes, err := opts.Confirm(ctx, 290_000_000)
				require.NoError(t, err)
				require.True(t, yes)
				if tc.remove {
					require.NoError(t, os.Remove(path))
				} else {
					require.NoError(t, os.WriteFile(path, []byte(tc.edited), 0600))
				}
				installed = true
				return embedlocal.Installation{Executable: "/example/kennembed"}, nil
			}
			out, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			got, readErr := os.ReadFile(path) // #nosec G304 -- path is derived from a test fixture
			if tc.remove {
				require.ErrorIs(t, readErr, os.ErrNotExist)
			} else {
				require.NoError(t, readErr)
				require.Equal(t, tc.edited, string(got))
			}
			require.ErrorContains(t, err, "changed")
			require.Empty(t, out)
		})
	}
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

func TestLocalEmbeddingsDaemonRefusesDownload(t *testing.T) {
	home := setupKataEnv(t)
	store := openKataTestDB(t, filepath.Join(home, "kata.db"))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "login crash", Author: "agent"})
	require.NoError(t, err)
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }
	localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		approved, err := opts.Confirm(ctx, 290_000_000)
		require.False(t, approved)
		return embedlocal.Installation{}, err
	}
	ec := config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text"}
	client, _, err := preflightEmbeddingStartup(ec, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	workers := newDaemonWorkerGroup()
	_, idx, health, _, err := startEmbeddingReconciler(ctx, workers, nil, ec, client, filepath.Join(home, "vectors.db"), store, daemon.NewEventBroadcaster(), log.New(io.Discard, "", 0))
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		require.True(t, workers.Wait(context.WithoutCancel(t.Context())))
		require.NoError(t, idx.Close())
	})
	//nolint:kennlint // sleeptest: observe the asynchronous backfill failure.
	require.Eventually(t, func() bool { return health().LastError != "" }, 10*time.Second, 10*time.Millisecond)
	require.Contains(t, health().LastError, "kata daemon local-embeddings")
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	require.Contains(t, health().LastError, dir)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Embedder: client, ReconcilerHealth: health})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), httpServer.URL), "health", "--json")
	require.NoError(t, err, stderr)
	require.Contains(t, out, `"error_present":true`)
	require.Contains(t, out, `"backlog":1`)
}

func TestLocalEmbeddingsSwitchKeepsIndex(t *testing.T) {
	home := setupKataEnv(t)
	stubLocalEmbeddingsInstall(t)
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
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("installed=%t", installed), func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_SERVER", "")
			path := filepath.Join(home, "config.toml")
			original := []byte("[search.embeddings]\nbase_url = 'http://127.0.0.1:11434/v1'\nmodel = 'mxbai-embed-large'\n")
			require.NoError(t, os.WriteFile(path, original, 0600))
			oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
			t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
			starts := 0
			localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
				if opts.Executable == "" {
					return "", embedlocal.ErrNotFound
				}
				starts++
				return "http://127.0.0.1:12345/v1", nil
			}
			localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				if !installed {
					approved, err := opts.Confirm(ctx, 290_000_000)
					require.False(t, approved)
					return embedlocal.Installation{}, err
				}
				return embedlocal.Installation{Executable: "/example/kennembed"}, nil
			}
			_, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings")
			require.ErrorContains(t, err, "no TTY")
			require.Zero(t, starts, "server must not start before the switch is approved")
			require.Contains(t, stderr, "index will be rebuilt")
			require.Contains(t, stderr, "replicas need the same recipe")
			got, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
			require.NoError(t, err)
			require.Equal(t, original, got)
		})
	}
}

func TestLocalEmbeddingsExternalCredential(t *testing.T) {
	home := setupKataEnv(t)
	stubLocalEmbeddingsInstall(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_API_KEY_FILE", filepath.Join(home, "unavailable-provider-key"))
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Empty(t, r.Header.Get("Authorization"))
		vector := make([]float32, 768)
		vector[0] = 1
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"embedding": vector}}}))
	}))
	t.Cleanup(provider.Close)
	oldEndpoint := localEmbeddingsEndpoint
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return provider.URL, nil }
	out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "configured in config.toml")
	require.Contains(t, stderr, "Service environment settings can override")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	client, _, err := preflightEmbeddingStartup(cfg.Embeddings, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = client.EmbedQuery(t.Context(), []string{"login crash"})
	require.NoError(t, err, "setup reported success but keyless local embedding is blocked")
	require.NoError(t, reloadEmbeddingCredentials(cfg.Embeddings, client, nil))
	_, err = client.EmbedQuery(t.Context(), []string{"login crash"})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	cfg.Embeddings.BaseURL = provider.URL
	external, _, err := preflightEmbeddingStartup(cfg.Embeddings, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = external.EmbedQuery(t.Context(), []string{"login crash"})
	require.ErrorContains(t, err, "no embedding API key")
	require.Equal(t, 2, calls)
}

func TestLocalEmbeddingsReplacementEnvironment(t *testing.T) {
	binary := kataTestBinary(t, "review-local-environment")
	home := setupKataEnv(t)
	stubLocalEmbeddingsInstall(t)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "")
	original := []byte("[search.embeddings]\nbase_url = 'http://127.0.0.1:11434/v1'\nmodel = 'nomic-embed-text'\n")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, original, 0600))
	oldEndpoint := localEmbeddingsEndpoint
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
	out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "configured in config.toml")
	require.Contains(t, stderr, "Service environment settings can override")
	written, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration snapshot
	require.NoError(t, err)
	require.NotEqual(t, original, written)
	// A service's replacement check inherits the service environment, not the invoking shell.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		require.Equal(t, "mxbai-embed-large", body.Model)
		vector := make([]float32, 768)
		vector[0] = 1
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"embedding": vector}}}))
	}))
	t.Cleanup(provider.Close)
	t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", provider.URL)
	t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "mxbai-embed-large")
	args := os.Args
	os.Args = []string{binary, "daemon", "start", "--foreground"}
	t.Cleanup(func() { os.Args = args })
	require.NoError(t, validateDaemonReplacement(t.Context(), binary, nil), "persisted setup configuration must be usable by the replacement")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.False(t, cfg.Embeddings.UsesLocal())
	require.Equal(t, "mxbai-embed-large", cfg.Embeddings.Model)
	localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
		t.Fatal("configured server must bypass local discovery")
		return "", nil
	}
	client, _, err := preflightEmbeddingStartup(cfg.Embeddings, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = client.EmbedQuery(t.Context(), []string{"login crash"})
	require.NoError(t, err)
}

func TestLocalEmbeddingsRestartRollback(t *testing.T) {
	binary := kataTestBinary(t, "local-rollback")
	childEnv := daemonLifecycleEnvironment(t)
	for _, scenario := range []string{"existing", "new", "concurrent edit"} {
		t.Run(scenario, func(t *testing.T) {
			home := setupKataEnv(t)
			stubLocalEmbeddingsInstall(t)
			path := filepath.Join(home, "config.toml")
			original := []byte("# original\n[search.embeddings]\nbase_url = 'http://127.0.0.1:11434/v1'\nmodel = 'nomic-embed-text'\n")
			env := append(slices.Clone(childEnv), "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"), "KATA_WORKSPACE="+t.TempDir())
			if scenario == "new" {
				env = append(env, "KATA_SEARCH_EMBEDDINGS_DIMS=769")
			} else {
				require.NoError(t, os.WriteFile(path, original, 0600))
				env = append(env, "KATA_SEARCH_EMBEDDINGS_MODEL=mxbai-embed-large")
			}
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			store := kitdaemon.RuntimeStore{Dir: ns.DataDir}
			record := func() kitdaemon.RuntimeRecord {
				records, err := store.List()
				require.NoError(t, err)
				for _, rec := range records {
					if daemonruntime.RuntimeProcessAlive(rec) {
						return rec
					}
				}
				return kitdaemon.RuntimeRecord{}
			}
			process := exec.CommandContext(context.WithoutCancel(t.Context()), binary, "daemon", "start", "--foreground", "--listen", "127.0.0.1:0")
			process.Env = env
			process.Stderr = os.Stderr
			require.NoError(t, process.Start())
			exited := make(chan error, 1)
			go func() { exited <- process.Wait() }()
			t.Cleanup(func() {
				stop := exec.CommandContext(context.WithoutCancel(t.Context()), binary, "daemon", "stop")
				stop.Env = env
				output, err := stop.CombinedOutput()
				require.NoError(t, err, string(output))
				select {
				case <-exited:
				case <-time.After(10 * time.Second):
					_ = process.Process.Kill()
					<-exited
					t.Error("daemon did not stop")
				}
			})
			//nolint:kennlint // sleeptest: wait for the isolated child to publish readiness.
			require.Eventually(t, func() bool { return record().PID != 0 }, 10*time.Second, 20*time.Millisecond)
			before := record()
			oldEndpoint := localEmbeddingsEndpoint
			t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
			oldExecutable := resolveDaemonExecutable
			t.Cleanup(func() { resolveDaemonExecutable = oldExecutable })
			edited := []byte("# edited while restart failed\n")
			resolveDaemonExecutable = func() (string, error) {
				if scenario == "concurrent edit" {
					require.NoError(t, os.WriteFile(path, edited, 0600))
					return "", errors.New("replacement unavailable")
				}
				return binary, nil
			}
			out, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			require.Error(t, err)
			require.Empty(t, out)
			got, readErr := os.ReadFile(path) // #nosec G304 -- test-owned configuration snapshot
			switch scenario {
			case "new":
				require.ErrorContains(t, err, "configuration restored")
				require.ErrorIs(t, readErr, os.ErrNotExist)
			case "concurrent edit":
				require.NoError(t, readErr)
				require.Equal(t, edited, got)
			default:
				require.ErrorContains(t, err, "configuration restored")
				require.NoError(t, readErr)
				require.Equal(t, original, got)
			}
			require.Equal(t, before.PID, record().PID)
		})
	}
}

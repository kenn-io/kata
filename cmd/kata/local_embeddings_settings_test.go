package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalEmbeddingsSetupStatus(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := t.TempDir()
	setLocalEmbeddingsTestHome(t, home)
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
	for _, tc := range []struct {
		name     string
		postgres func(*testing.T, context.Context) (string, func())
		ready    bool
	}{
		{"SQLite", nil, true},
		{"Postgres without pgvector", testenv.NewPlainPostgresContainer, false},
		{"Postgres with pgvector", testenv.NewPostgresWithPgvectorContainer, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearLocalEmbeddingsEnv(t)
			resetFlags(t)
			home := setupKataEnv(t)
			setLocalEmbeddingsTestHome(t, t.TempDir())
			t.Setenv("PORT", "")
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_DSN", "")
			t.Setenv("KATA_AUTH_TOKEN", "")
			t.Setenv(daemonruntime.AutoStartMarkerEnv, "1")
			if tc.postgres != nil {
				if testing.Short() {
					t.Skip("requires postgres testcontainer")
				}
				dsn, cleanup := tc.postgres(t, t.Context())
				t.Cleanup(cleanup)
				t.Setenv("KATA_DSN", dsn)
				t.Setenv("KATA_POSTGRES_ALLOW_INSECURE", "1")
			}
			path := filepath.Join(home, "config.toml")
			original := []byte("# example configuration\n[search.embeddings]\ndims = 1024\n")
			require.NoError(t, os.WriteFile(path, original, 0o600))
			oldTelemetry := newTelemetryReporter
			newTelemetryReporter = func(telemetry.Options) telemetry.Client { return &fakeTelemetryReporter{} }
			t.Cleanup(func() { newTelemetryReporter = oldTelemetry })
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Input []string `json:"input"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
				vectors := make([]map[string]any, len(input.Input))
				for i := range input.Input {
					v := make([]float64, 768)
					v[0] = 1
					vectors[i] = map[string]any{"embedding": v, "index": i}
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": vectors}))
			}))
			t.Cleanup(provider.Close)
			oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
			t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
			var installs atomic.Int32
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
				if installs.Load() == 0 {
					return "", embedlocal.ErrNotFound
				}
				return provider.URL, nil
			}
			localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
				installs.Add(1)
				return embedlocal.Installation{Executable: "/example/kennembed"}, nil
			}
			launch := func() (string, func()) {
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- runDaemonWithListen(ctx, "127.0.0.1:0", false, false) }()
				var once sync.Once
				stop := func() {
					once.Do(func() {
						cancel()
						select {
						case err := <-done:
							require.NoError(t, err)
						case <-time.After(30 * time.Second):
							t.Fatal("daemon shutdown timed out")
						}
					})
				}
				t.Cleanup(stop)
				namespace, err := daemonruntime.NewNamespace()
				require.NoError(t, err)
				runtimePath, err := (kitdaemon.RuntimeStore{Dir: namespace.DataDir}).Path(os.Getpid())
				require.NoError(t, err)
				var record daemonRuntimeRecordJSON
				require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: observes the daemon's runtime publication
					body, err := os.ReadFile(runtimePath) // #nosec G304 -- isolated test runtime path
					return err == nil && json.Unmarshal(body, &record) == nil
				}, 5*time.Second, 10*time.Millisecond)
				return "http://" + record.Address, stop
			}
			client := &http.Client{Timeout: time.Second}
			baseURL, stop := launch()
			request := func(method, path, body string, expected int, output any) {
				req, err := http.NewRequestWithContext(t.Context(), method, baseURL+path, strings.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				response, err := client.Do(req)
				require.NoError(t, err)
				defer func() { _ = response.Body.Close() }()
				require.Equal(t, expected, response.StatusCode)
				if output != nil {
					require.NoError(t, json.NewDecoder(response.Body).Decode(output))
				}
			}
			var project struct {
				Project struct {
					ID int64 `json:"id"`
				} `json:"project"`
			}
			request("POST", "/api/v1/projects", `{"name":"example-project","actor":"tester"}`, 200, &project)
			request("POST", fmt.Sprintf("/api/v1/projects/%d/issues", project.Project.ID), `{"title":"Repair the bicycle brakes","actor":"tester"}`, 200, nil)
			request("POST", "/api/v1/embeddings/local", `{}`, 202, nil)
			var status api.LocalEmbeddingsStatus
			require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: observes the admitted setup worker
				request("GET", "/api/v1/embeddings/local", "", 200, &status)
				return status.Job != nil && (status.Job.State == "restart" || status.Job.State == "failed")
			}, 5*time.Second, 10*time.Millisecond)
			require.Equal(t, "none", status.Provider, "the running daemon keeps its startup configuration")
			data, err := os.ReadFile(path) // #nosec G304 -- configuration in the isolated test home
			require.NoError(t, err)
			if tc.ready {
				require.Equal(t, "restart", status.Job.State, status.Job.Error)
				chosen, err := embedlocal.ChoiceRecorded()
				require.NoError(t, err)
				require.True(t, chosen)
				require.False(t, status.CanChange)
			} else {
				require.Equal(t, "failed", status.Job.State)
				require.Contains(t, status.Job.Error, "pgvector")
				require.Zero(t, installs.Load())
				chosen, err := embedlocal.ChoiceRecorded()
				require.NoError(t, err)
				require.False(t, chosen)
				require.Equal(t, original, data)
				require.True(t, status.CanChange, "the owner can retry after database preparation")
			}
			search := func() (string, int, error) {
				out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), "--json", "--project", "example-project", "search", "bicycle")
				if err != nil {
					return "", 0, fmt.Errorf("search: %w: %s", err, stderr)
				}
				var result struct {
					Mode    string `json:"mode"`
					Results []any  `json:"results"`
				}
				err = json.Unmarshal([]byte(out), &result)
				return result.Mode, len(result.Results), err
			}
			stop()
			if !tc.ready {
				require.NoError(t, embedlocal.RecordChoice())
			}
			if tc.ready {
				// Simulate another app recording the choice before this daemon starts.
				require.NoError(t, os.WriteFile(path, original, 0o600))
				require.NoError(t, os.Chmod(path, 0o400))
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			}
			baseURL, _ = launch()
			if tc.ready {
				request("GET", "/api/v1/embeddings/local", "", 200, &status)
				require.Equal(t, "built_in", status.Provider)
				saved, err := os.ReadFile(path) // #nosec G304 -- isolated test configuration
				require.NoError(t, err)
				require.Equal(t, original, saved)
			}
			if !tc.ready {
				saved, err := os.ReadFile(path) // #nosec G304 -- isolated test configuration
				require.NoError(t, err)
				require.Equal(t, original, saved)
			}
			wantMode := "lexical"
			if tc.ready {
				wantMode = "hybrid"
			}
			require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: waits for the real reconciler after explicit restart
				mode, count, err := search()
				return err == nil && mode == wantMode && count == 1
			}, 10*time.Second, 20*time.Millisecond)
		})
	}
}

func TestLocalEmbeddingsTurnOnPreservesProviderSavedAfterStartup(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	setLocalEmbeddingsTestHome(t, t.TempDir())
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
	chosen, err := embedlocal.ChoiceRecorded()
	require.NoError(t, err)
	require.False(t, chosen)
}

func TestSharedChoiceEnablesStartup(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	setLocalEmbeddingsTestHome(t, t.TempDir())
	require.NoError(t, embedlocal.RecordChoice())
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	next, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	require.True(t, next.UsesLocal())
	_, err = os.Stat(filepath.Join(home, "config.toml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSharedChoicePreservesStartupProviders(t *testing.T) {
	for _, tc := range []struct {
		name, file, env string
		choice          bool
	}{
		{name: "no choice"},
		{name: "file provider", file: "[search.embeddings]\nbase_url = 'https://embeddings.example/v1'\nmodel = 'example'\ndims = 1024\n", choice: true},
		{name: "environment provider", env: "https://embeddings.example/v1", choice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearLocalEmbeddingsEnv(t)
			home := setupKataEnv(t)
			setLocalEmbeddingsTestHome(t, t.TempDir())
			if tc.env != "" {
				t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", tc.env)
				t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "example")
				t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", "768")
			}
			path := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.file), 0o600))
			if tc.choice {
				require.NoError(t, embedlocal.RecordChoice())
			}
			cfg, err := config.ReadSearchConfig()
			require.NoError(t, err)
			next, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
			require.NoError(t, err)
			require.Equal(t, cfg.Embeddings, next)
			data, err := os.ReadFile(path) // #nosec G304 -- isolated test configuration
			require.NoError(t, err)
			require.Equal(t, tc.file, string(data))
		})
	}
}

func TestSavedLocalDoesNotRecordSharedChoice(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	setupKataEnv(t)
	setLocalEmbeddingsTestHome(t, t.TempDir())
	cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\nlocal = true\n"))
	require.NoError(t, err)
	next, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	require.Equal(t, cfg.Embeddings, next)
	chosen, err := embedlocal.ChoiceRecorded()
	require.NoError(t, err)
	require.False(t, chosen)
}

func TestSharedChoiceRejectsInvalidBatchBeforeSaving(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	setLocalEmbeddingsTestHome(t, t.TempDir())
	require.NoError(t, embedlocal.RecordChoice())
	original := "[search.embeddings]\nmodel_context_tokens = 32000\nmax_batch_tokens = 31999\n"
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	next, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	_, _, err = preflightEmbeddingStartup(next, filepath.Join(home, "kata.db"))
	require.ErrorContains(t, err, "embedding batching")
	data, err := os.ReadFile(path) // #nosec G304 -- isolated test configuration
	require.NoError(t, err)
	require.Equal(t, original, string(data))
}

func TestSavedLocalKeepsExistingChoiceFile(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	setupKataEnv(t)
	setLocalEmbeddingsTestHome(t, t.TempDir())
	require.NoError(t, embedlocal.RecordChoice())
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	path := filepath.Join(dir, "search-by-meaning")
	old := time.Unix(1000000000, 0)
	require.NoError(t, os.Chtimes(path, old, old))
	cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\nlocal = true\n"))
	require.NoError(t, err)
	_, err = applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, old, info.ModTime())
}

func TestSavedLocalStartsWhenChoiceCannotBeWritten(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	resetFlags(t)
	home := setupKataEnv(t)
	sharedHome := t.TempDir()
	setLocalEmbeddingsTestHome(t, sharedHome)
	t.Setenv("PORT", "")
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, os.WriteFile(filepath.Join(sharedHome, ".kenn"), []byte("blocks marker directory"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[search.embeddings]\nlocal = true\n"), 0o600))
	oldOpen := openEmbeddingVectorIndex
	openEmbeddingVectorIndex = func(context.Context, db.Storage, string) (*vector.Index, error) {
		return nil, errors.New("embedding index reached")
	}
	t.Cleanup(func() { openEmbeddingVectorIndex = oldOpen })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := runDaemonWithListen(ctx, "127.0.0.1:0", false, false)
	require.ErrorContains(t, err, "embedding index reached")
}

func TestTurnOnDoesNotSaveLocalWhenChoiceCannotBeWritten(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	sharedHome := t.TempDir()
	setLocalEmbeddingsTestHome(t, sharedHome)
	require.NoError(t, os.WriteFile(filepath.Join(sharedHome, ".kenn"), []byte("blocks marker directory"), 0o600))
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		if opts.Executable == "" {
			return "", embedlocal.ErrNotFound
		}
		return "http://127.0.0.1:12345/v1", nil
	}
	localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
	_, err := enableLocalEmbeddings(t.Context(), localEmbeddingsRun{
		confirm: func(context.Context, int64) (bool, error) { return true, nil },
		notice:  func(string) error { return nil },
		restart: func(_ context.Context, apply func() error) error { return apply() },
	})
	require.ErrorContains(t, err, "record built-in embeddings choice")
	_, err = os.Stat(filepath.Join(home, "config.toml"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTurnOnKeepsExistingChoiceFile(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	setupKataEnv(t)
	setLocalEmbeddingsTestHome(t, t.TempDir())
	require.NoError(t, embedlocal.RecordChoice())
	dir, err := embedlocal.DefaultDir()
	require.NoError(t, err)
	path := filepath.Join(dir, "search-by-meaning")
	old := time.Unix(1000000000, 0)
	require.NoError(t, os.Chtimes(path, old, old))
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		if opts.Executable == "" {
			return "", embedlocal.ErrNotFound
		}
		return "http://127.0.0.1:12345/v1", nil
	}
	localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
	_, err = enableLocalEmbeddings(t.Context(), localEmbeddingsRun{
		confirm: func(context.Context, int64) (bool, error) { return true, nil },
		notice:  func(string) error { return nil },
		restart: func(_ context.Context, apply func() error) error { return apply() },
	})
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, old, info.ModTime())
}

func TestSavedLocalUsesInstalledServerWithoutRecordingChoice(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	setLocalEmbeddingsTestHome(t, t.TempDir())
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		values := make([]float64, 768)
		values[0] = 1
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": values, "index": 0}}}))
	}))
	t.Cleanup(provider.Close)
	oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
	t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
	localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		if opts.Executable == "" {
			return "", embedlocal.ErrNotFound
		}
		return provider.URL, nil
	}
	localEmbeddingsInstall = func(_ context.Context, _ embedlocal.InstallOptions) (embedlocal.Installation, error) {
		return embedlocal.Installation{Executable: "/example/kennembed"}, nil
	}
	cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\nlocal = true\n"))
	require.NoError(t, err)
	next, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	client, _, err := preflightEmbeddingStartup(next, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), []string{"example"})
	require.NoError(t, err)
	chosen, err := embedlocal.ChoiceRecorded()
	require.NoError(t, err)
	require.False(t, chosen)
}

func TestSharedChoiceUsesBuiltInDimensionsForInactiveConfig(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	require.NoError(t, embedlocal.RecordChoice())
	original := []byte("[search.embeddings]\ndims = 1024\n")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	next, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	require.True(t, next.UsesLocal())
	require.Equal(t, "nomic-embed-text", next.Model)
	require.Equal(t, 768, next.Dims)
	data, err := os.ReadFile(path) // #nosec G304 -- isolated test configuration
	require.NoError(t, err)
	require.Equal(t, original, data)
}

package main

import (
	"context"
	"encoding/json"
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
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kata/internal/testenv"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalEmbeddingsSetupStatus(t *testing.T) {
	home := clearLocalEmbeddingsEnv(t)
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
			original := []byte("# example configuration\n")
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
				require.False(t, status.CanChange)
			} else {
				require.Equal(t, "failed", status.Job.State)
				require.Contains(t, status.Job.Error, "pgvector")
				require.Zero(t, installs.Load())
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
			baseURL, _ = launch()
			wantMode := "lexical"
			if tc.ready {
				saved, err := os.ReadFile(path) // #nosec G304 -- isolated test configuration
				require.NoError(t, err)
				require.Equal(t, data, saved, "startup preserves the saved configuration")
				request("GET", "/api/v1/embeddings/local", "", 200, &status)
				require.Equal(t, "built_in", status.Provider)
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

func TestSharedChoiceDaemonStartup(t *testing.T) {
	for _, scenario := range []string{"vector path directory", "preflight", "enabled"} {
		t.Run(scenario, func(t *testing.T) {
			clearLocalEmbeddingsEnv(t)
			resetFlags(t)
			home := setupKataEnv(t)
			t.Setenv("PORT", "")
			t.Setenv("KATA_DSN", "")
			t.Setenv("KATA_AUTH_TOKEN", "")
			t.Setenv(daemonruntime.AutoStartMarkerEnv, "1")
			require.NoError(t, embedlocal.RecordChoice())
			switch scenario {
			case "vector path directory":
				path, err := vectorsPathForDSN(filepath.Join(home, "kata.db"))
				require.NoError(t, err)
				require.NoError(t, os.Mkdir(path, 0o700))
			case "preflight":
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[search.embeddings]\nmodel_context_tokens=32000\nmax_batch_tokens=31999\n"), 0o600))
				cfg, err := config.ReadDaemonConfig()
				require.NoError(t, err)
				chosen, err := applySharedEmbeddingChoice(t.Context(), cfg.Search.Embeddings, filepath.Join(home, "kata.db"))
				require.NoError(t, err)
				_, _, err = preflightEmbeddingStartup(chosen, filepath.Join(home, "kata.db"))
				require.ErrorContains(t, err, "embedding batching")
			}
			if scenario == "enabled" {
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
				oldEndpoint := localEmbeddingsEndpoint
				t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
				localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return provider.URL, nil }
			}
			baseURL := launchDaemon(t)
			status := getJSON[api.LocalEmbeddingsStatus](t, baseURL+"/api/v1/embeddings/local")
			if scenario == "enabled" {
				require.Equal(t, "built_in", status.Provider)
			} else {
				require.Equal(t, "none", status.Provider)
			}
			project := postJSON[struct {
				Project struct {
					ID int64 `json:"id"`
				} `json:"project"`
			}](t, baseURL+"/api/v1/projects", map[string]string{"name": "example-project", "actor": "user-a"})
			projectURL := fmt.Sprintf("%s/api/v1/projects/%d", baseURL, project.Project.ID)
			postJSONOK(t, projectURL+"/issues", map[string]string{"title": "Repair bicycle", "actor": "user-a"})
			result := getJSON[struct {
				Mode    string          `json:"mode"`
				Results []api.SearchHit `json:"results"`
			}](t, projectURL+"/search?q=bicycle")
			if scenario == "enabled" {
				require.Equal(t, "hybrid", result.Mode)
			} else {
				require.Equal(t, "lexical", result.Mode)
			}
			require.Len(t, result.Results, 1)
			require.Equal(t, "Repair bicycle", result.Results[0].Issue.Title)
		})
	}
}

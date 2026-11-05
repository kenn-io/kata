package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
)

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

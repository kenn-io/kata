package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	_, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err)
	require.Zero(t, installs, "a fresh home must use the ready Ollama without installation")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:11434/v1", cfg.Embeddings.BaseURL)
	require.Equal(t, "nomic-embed-text", cfg.Embeddings.Model)
	require.False(t, cfg.Embeddings.Local)
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
	_, stderr, err := executeRootCapture(t, t.Context(), "federation", "embedding-recipe")
	require.NoError(t, err, stderr)
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
}

func TestLocalEmbeddingsSwitchRecipe(t *testing.T) {
	for _, salt := range []string{"", "changed-nomic-weights"} {
		t.Run("salt="+salt, func(t *testing.T) {
			home := setupKataEnv(t)
			stubLocalEmbeddingsInstall(t)
			t.Setenv("KATA_SERVER", "")
			original := fmt.Sprintf("# shared config\n[search.embeddings]\n# keep recipe\nbase_url = %q\nmodel = 'nomic-embed-text'\ndims = 768\ndocument_prefix = 'search_document: '\nquery_prefix = 'search_query: '\nfingerprint_salt = %q\n[display]\n# untouched\n", "http://127.0.0.1:11434/v1", salt)
			path := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(original), 0640)) // #nosec G306 -- fixture exercises preservation of an existing mode
			oldEndpoint := localEmbeddingsEndpoint
			localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
				require.True(t, opts.SkipOllama)
				return "http://127.0.0.1:12345/v1", nil
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
				} else if salt == "" {
					require.Equal(t, firstFingerprint, client.Generation().Fingerprint())
					require.Equal(t, firstRecipe, recipe)
				} else {
					require.Empty(t, ec.Embeddings.FingerprintSalt)
					require.NotEqual(t, firstFingerprint, client.Generation().Fingerprint())
					require.NotEqual(t, firstRecipe.RecipeFingerprint, recipe.RecipeFingerprint)
				}
				if phase == 1 {
					require.True(t, ec.Embeddings.UsesLocal())
					continue
				}
				out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
				require.NoError(t, err, out+stderr)
				if salt == "" {
					require.Contains(t, stderr, "index will be kept")
				} else {
					require.Contains(t, stderr, "index will be rebuilt")
				}
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0640), info.Mode().Perm())
			}
		})
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

func TestLocalEmbeddingsReplacementEnvironment(t *testing.T) {
	binary := kataTestBinary(t, "review-local-environment")
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[search.embeddings]\nlocal = true\n"), 0600))
	// A service's replacement check inherits its configured provider.
	t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "http://127.0.0.1:11434/v1")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "mxbai-embed-large")
	args := os.Args
	os.Args = []string{binary, "daemon", "start", "--foreground"}
	t.Cleanup(func() { os.Args = args })
	require.NoError(t, validateDaemonReplacement(t.Context(), binary, nil), "persisted setup configuration must be usable by the replacement")
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

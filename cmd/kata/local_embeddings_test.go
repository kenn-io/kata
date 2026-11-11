package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
	kitvec "go.kenn.io/kit/vector"
)

func TestApplySharedEmbeddingChoice(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	require.NoError(t, embedlocal.RecordChoice())
	cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\nbase_url='https://embeddings.example/v1'\nmodel='example-model'\ndims=1024\n"))
	require.NoError(t, err)
	got, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	require.Equal(t, cfg.Embeddings, got)
	cfg, err = config.ParseSearchConfig([]byte("[search.embeddings]\ndims=1024\ndocument_prefix='document prefix'\ndocument_suffix='document suffix'\nquery_prefix='query prefix'\nquery_suffix='query suffix'\nfingerprint_salt='salt'\nrequest_dimensions=true\n"))
	require.NoError(t, err)
	got, err = applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	require.Equal(t, config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text", Dims: 768}, got)
}

func TestSharedChoiceStartsPostgresCompatibleIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	for _, mode := range []string{"fresh", "compatible", "other"} {
		t.Run(mode, func(t *testing.T) {
			setupKataEnv(t)
			clearLocalEmbeddingsEnv(t)
			resetFlags(t)
			t.Setenv("PORT", "")
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_AUTH_TOKEN", "")
			t.Setenv(daemonruntime.AutoStartMarkerEnv, "1")
			dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, t.Context())
			t.Cleanup(cleanup)
			t.Setenv("KATA_DSN", dsn)
			t.Setenv("KATA_POSTGRES_ALLOW_INSECURE", "1")
			store, err := pgstore.Open(t.Context(), dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			idx, err := vector.OpenPostgres(t.Context(), store.DB)
			require.NoError(t, err)
			require.NoError(t, embedlocal.RecordChoice())
			choice, err := applySharedEmbeddingChoice(t.Context(), config.EmbeddingsConfig{})
			require.NoError(t, err)
			emb, _, err := preflightEmbeddingStartup(choice, dsn)
			require.NoError(t, err)
			generation := emb.Generation()
			if mode == "other" {
				generation = kitvec.Generation{Model: "external-model", Dimensions: 768}
			}
			key := generation.Fingerprint()
			if mode != "fresh" {
				release, err := idx.AcquireReconcilerLease(t.Context())
				require.NoError(t, err)
				require.NoError(t, idx.EnsureBuilding(t.Context(), key, generation))
				require.NoError(t, idx.CutOver(t.Context(), key))
				require.NoError(t, release())
			}
			baseURL := launchDaemon(t)
			status := getJSON[api.LocalEmbeddingsStatus](t, baseURL+"/api/v1/embeddings/local")
			require.Equal(t, "built_in", status.Provider)
			//nolint:kennlint // sleeptest: The daemon publishes indexing progress through HTTP.
			require.Eventually(t, func() bool {
				status := getJSON[api.LocalEmbeddingsStatus](t, baseURL+"/api/v1/embeddings/local")
				if mode == "other" {
					return status.Indexing != nil && status.Indexing.Failed
				}
				return status.Indexing == nil
			}, 5*time.Second, 10*time.Millisecond)
			//nolint:kennlint // sleeptest: PostgreSQL exposes the reconciler's active generation through database reads.
			require.Eventually(t, func() bool {
				active, ok, err := idx.ActiveGeneration(t.Context())
				return err == nil && ok && active == key
			}, 5*time.Second, 10*time.Millisecond)
			if mode == "other" {
				//nolint:kennlint // sleeptest: Observe the active generation over time while the daemon runs.
				require.Never(t, func() bool {
					active, ok, err := idx.ActiveGeneration(t.Context())
					return err != nil || !ok || active != key
				}, 2*time.Second, 10*time.Millisecond)
			}
		})
	}
}

func TestLocalEmbeddingsSetup(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	tmp := setupKataEnv(t)
	home := filepath.Join(tmp, "fresh")
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	t.Setenv("KATA_SERVER", "")
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
		expectedDir, err := embedlocal.DefaultDir()
		require.NoError(t, err)
		require.Equal(t, expectedDir, opts.DataDir)
		yes, err := opts.Confirm(ctx, 290_000_000)
		require.NoError(t, err)
		require.True(t, yes)
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
	marker := filepath.Join(tmp, ".kenn", "embeddings", "search-by-meaning")
	old := time.Unix(1000000000, 0)
	require.NoError(t, os.Chtimes(marker, old, old))
	_, stderr, err = executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err, stderr)
	info, err := os.Stat(marker)
	require.NoError(t, err)
	require.Equal(t, old, info.ModTime())
}

func TestLocalEmbeddingsMarkerWriteFailure(t *testing.T) {
	setupKataEnv(t)
	home := clearLocalEmbeddingsEnv(t)
	stubLocalEmbeddingsInstall(t)
	oldEndpoint := localEmbeddingsEndpoint
	t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
	localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
		if opts.Executable == "" {
			return "", embedlocal.ErrNotFound
		}
		return "http://127.0.0.1:12345/v1", nil
	}
	marker := filepath.Join(home, ".kenn", "embeddings", "search-by-meaning")
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0700))
	if err := os.Symlink("missing", marker); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	run := localEmbeddingsRun{
		confirm: func(context.Context, int64) (bool, error) { return true, nil },
		notice:  func(string) error { return nil },
		restart: func(_ context.Context, apply func() error) error { return apply() },
	}
	_, err := enableLocalEmbeddings(t.Context(), run)
	require.ErrorContains(t, err, "sharing could not be confirmed")
	require.ErrorContains(t, err, "record search by meaning choice")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.Local)
}

func TestLocalEmbeddingsSetupRefusesBeforeInstall(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	for _, scenario := range []string{"environment override", "dangling symlink"} {
		t.Run(scenario, func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "")
			oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
			t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
				t.Fatal("setup refusal must precede server discovery")
				return "", nil
			}
			localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
				t.Fatal("setup refusal must precede installation")
				return embedlocal.Installation{}, nil
			}
			original := []byte("# untouched\n")
			path := filepath.Join(home, "config.toml")
			if scenario == "dangling symlink" {
				if err := os.Symlink("provider.toml", path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				t.Setenv("KATA_SEARCH_EMBEDDINGS_BASE_URL", "http://127.0.0.1:11434/v1")
				t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "nomic-embed-text")
				require.NoError(t, os.WriteFile(path, original, 0600))
			}
			_, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			if scenario == "dangling symlink" {
				require.ErrorContains(t, err, "symlink")
				link, err := os.Readlink(path)
				require.NoError(t, err)
				require.Equal(t, "provider.toml", link)
				_, err = os.Stat(filepath.Join(home, "provider.toml"))
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.ErrorContains(t, err, "KATA_SEARCH_EMBEDDINGS_BASE_URL")
			got, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
			require.NoError(t, err)
			require.Equal(t, original, got)
		})
	}
}

func TestLocalEmbeddingsFreshOllama(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	setupKataEnv(t)
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
	chosen, err := embedlocal.ChoiceRecorded()
	require.NoError(t, err)
	require.False(t, chosen)
}

func TestLocalEmbeddingsSetupRefusesChangedConfig(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	for _, tc := range []struct {
		name     string
		original string
		exists   bool
		edited   string
	}{
		{name: "edited", exists: true, original: "[daemon]\nport = 8888\n", edited: "[daemon]\nport = 9999\n"},
		{name: "created empty"},
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
				require.NoError(t, os.WriteFile(path, []byte(tc.edited), 0600))
				installed = true
				return embedlocal.Installation{Executable: "/example/kennembed"}, nil
			}
			out, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			require.Empty(t, out)
			got, readErr := os.ReadFile(path) // #nosec G304 -- path is derived from a test fixture
			require.NoError(t, readErr)
			require.Equal(t, tc.edited, string(got))
			require.ErrorContains(t, err, "changed")
		})
	}
}

func TestLocalEmbeddingsDaemonDownloadConsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chosen bool
	}{
		{name: "no choice"}, {name: "recorded choice", chosen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearLocalEmbeddingsEnv(t)
			home := setupKataEnv(t)
			if tc.chosen {
				require.NoError(t, embedlocal.RecordChoice())
			}
			oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
			t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
				return "", embedlocal.ErrNotFound
			}
			localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				approved, err := opts.Confirm(ctx, 290_000_000)
				require.Equal(t, tc.chosen, approved)
				if err != nil {
					return embedlocal.Installation{}, err
				}
				return embedlocal.Installation{}, errors.New("installation reached")
			}
			cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\nlocal = true\n"))
			require.NoError(t, err)
			client, _, err := preflightEmbeddingStartup(cfg.Embeddings, filepath.Join(home, "kata.db"))
			require.NoError(t, err)
			_, err = client.Embed(t.Context(), []string{"x"})
			if tc.chosen {
				require.ErrorContains(t, err, "installation reached")
			} else {
				require.ErrorContains(t, err, "kata daemon local-embeddings")
			}
		})
	}
}

func TestLocalEmbeddingsSwitchRecipe(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	for _, tt := range []struct {
		name, recipe, index string
		symlink             bool
	}{
		{name: "Nomic", recipe: "model='nomic-embed-text'\ndims=768\ndocument_prefix='search_document: '\nquery_prefix='search_query: '\n", index: "kept"},
		{name: "external revision", recipe: "model='nomic-embed-text'\ndims=768\nfingerprint_salt='changed-nomic-weights'\n", index: "rebuilt"},
		{name: "symlink", recipe: "model='nomic-embed-text'\ndims=768\n", index: "kept", symlink: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := setupKataEnv(t)
			stubLocalEmbeddingsInstall(t)
			t.Setenv("KATA_SERVER", "")
			path := filepath.Join(home, "config.toml")
			target := path
			if tt.symlink {
				target = filepath.Join(home, "provider.toml")
				if err := os.Symlink("provider.toml", path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			original := "[search.embeddings]\nbase_url='http://127.0.0.1:11434/v1'\n" + tt.recipe
			require.NoError(t, os.WriteFile(target, []byte(original), 0640)) // #nosec G306 -- fixture exercises preservation of an existing mode
			before, err := os.Stat(target)
			require.NoError(t, err)
			oldEndpoint := localEmbeddingsEndpoint
			localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
				require.True(t, opts.SkipOllama)
				return "http://127.0.0.1:12345/v1", nil
			}
			t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
			out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			require.NoError(t, err, out+stderr)
			require.Contains(t, stderr, "index will be "+tt.index)
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, before.Mode().Perm(), info.Mode().Perm())
			if tt.symlink {
				link, err := os.Readlink(path)
				require.NoError(t, err)
				require.Equal(t, "provider.toml", link)
			}
		})
	}
}

func TestLocalEmbeddingsRebuildRefusal(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	stubIsTTY(t, false)
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
			require.Contains(t, stderr, "Daemons sharing embeddings need the same model and settings before reusing them.")
			got, err := os.ReadFile(path) // #nosec G304 -- path is derived from KATA_HOME or a test fixture
			require.NoError(t, err)
			require.Equal(t, original, got)
		})
	}
}

func TestLocalEmbeddingsRestartRollback(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	for _, scenario := range []string{"existing symlink", "new", "concurrent edit", "unsupported daemon", "restart refused"} {
		t.Run(scenario, func(t *testing.T) {
			home := setupKataEnv(t)
			stubLocalEmbeddingsInstall(t)
			t.Setenv("KATA_SERVER", "")
			path := filepath.Join(home, "config.toml")
			original := []byte("# original\n[search.embeddings]\nbase_url = 'http://127.0.0.1:11434/v1'\nmodel = 'nomic-embed-text'\n")
			if scenario != "new" && scenario != "unsupported daemon" {
				target := path
				if scenario == "existing symlink" {
					target = filepath.Join(home, "provider.toml")
					if err := os.Symlink("provider.toml", path); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
				require.NoError(t, os.WriteFile(target, original, 0600))
			}
			if scenario == "restart refused" {
				oldEndpoint := localEmbeddingsEndpoint
				t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
				localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
				_, err := enableLocalEmbeddings(t.Context(), localEmbeddingsRun{
					confirm: func(context.Context, int64) (bool, error) { return true, nil },
					notice:  func(string) error { return nil },
					restart: func(_ context.Context, apply func() error) error {
						require.NoError(t, apply())
						return errors.New("restart refused")
					},
				})
				require.ErrorContains(t, err, "restart refused")
				got, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration
				require.NoError(t, err)
				require.Equal(t, original, got)
				chosen, err := embedlocal.ChoiceRecorded()
				require.NoError(t, err)
				require.False(t, chosen)
				return
			}
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
			require.True(t, ok)
			record := kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now(), Network: "tcp", Address: "127.0.0.1:12345"}
			if scenario != "unsupported daemon" {
				record.Metadata = map[string]string{daemonruntime.RuntimeRestartExecutableKey: "1"}
			}
			_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(record)
			require.NoError(t, err)
			oldEndpoint := localEmbeddingsEndpoint
			t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
			edited := []byte("# edited while restart failed\n")
			stubDaemonExecutable(t, func() (string, error) {
				if scenario == "unsupported daemon" {
					t.Fatal("unsupported daemon must be refused before resolving a replacement")
				}
				if scenario == "concurrent edit" {
					require.NoError(t, os.WriteFile(path, edited, 0600))
				}
				return "", errors.New("replacement unavailable")
			})
			out, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			if scenario == "unsupported daemon" {
				require.ErrorContains(t, err, "is from an older kata release")
				_, err = os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.ErrorContains(t, err, "cannot resolve the installed restart executable")
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
				link, err := os.Readlink(path)
				require.NoError(t, err)
				require.Equal(t, "provider.toml", link)
			}
		})
	}
}

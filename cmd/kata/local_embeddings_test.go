package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
)

func TestApplySharedEmbeddingChoice(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	require.NoError(t, embedlocal.RecordChoice())
	cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\nbase_url='https://embeddings.example/v1'\nmodel='example-model'\ndims=1024\n"))
	require.NoError(t, err)
	got, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings, "kata.db")
	require.NoError(t, err)
	require.Equal(t, cfg.Embeddings, got)
	cfg, err = config.ParseSearchConfig([]byte("[search.embeddings]\ndims=1024\ndocument_prefix='document prefix'\ndocument_suffix='document suffix'\nquery_prefix='query prefix'\nquery_suffix='query suffix'\nfingerprint_salt='salt'\nrequest_dimensions=true\n"))
	require.NoError(t, err)
	got, err = applySharedEmbeddingChoice(t.Context(), cfg.Embeddings, "kata.db")
	require.NoError(t, err)
	require.Equal(t, config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text", Dims: 768}, got)
	got, err = applySharedEmbeddingChoice(t.Context(), config.EmbeddingsConfig{}, "postgres://example.invalid/kata")
	require.NoError(t, err)
	require.Equal(t, config.EmbeddingsConfig{}, got)
}

func TestLocalEmbeddingsSetup(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		clearLocalEmbeddingsEnv(t)
		home := setupKataEnv(t)
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
		chosen, err := embedlocal.ChoiceRecorded()
		require.NoError(t, err)
		require.True(t, chosen)
	})
	t.Run("marker write failure", func(t *testing.T) {
		setupKataEnv(t)
		home := clearLocalEmbeddingsEnv(t)
		t.Setenv("KATA_SERVER", "")
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
		require.NoError(t, os.MkdirAll(marker, 0o700))
		var logs bytes.Buffer
		original := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(original) })
		_, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
		require.NoError(t, err)
		require.Contains(t, logs.String(), "could not confirm that the shared search by meaning choice was saved")
		require.Contains(t, logs.String(), "record search by meaning choice")
		cfg, err := config.ReadSearchConfig()
		require.NoError(t, err)
		require.True(t, cfg.Embeddings.Local)
	})
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
		name     string
		chosen   bool
		postgres bool
	}{
		{name: "no choice"}, {name: "recorded choice", chosen: true}, {name: "postgres recorded choice", chosen: true, postgres: true},
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
				require.Equal(t, tc.chosen && !tc.postgres, approved)
				if err != nil {
					return embedlocal.Installation{}, err
				}
				return embedlocal.Installation{}, errors.New("installation reached")
			}
			cfg, err := config.ParseSearchConfig([]byte("[search.embeddings]\nlocal = true\n"))
			require.NoError(t, err)
			dsn := filepath.Join(home, "kata.db")
			if tc.postgres {
				dsn = "postgres://example.invalid/kata"
			}
			client, _, err := preflightEmbeddingStartup(cfg.Embeddings, dsn)
			require.NoError(t, err)
			_, err = client.Embed(t.Context(), []string{"x"})
			if tc.chosen && !tc.postgres {
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
	for _, scenario := range []string{"existing symlink", "new", "concurrent edit", "unsupported daemon"} {
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
			chosen, choiceErr := embedlocal.ChoiceRecorded()
			require.NoError(t, choiceErr)
			require.False(t, chosen)
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

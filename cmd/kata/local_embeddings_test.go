package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
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
		if installed {
			return embedlocal.Installation{Executable: "/example/kennembed"}, nil
		}
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
	require.True(t, cfg.Embeddings.Enabled())
	out, _, err = executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "index kept")
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

func TestLocalEmbeddingsFreshOllama(t *testing.T) {
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
	_, err = client.Embed(t.Context(), []string{"x"})
	require.ErrorContains(t, err, "kata daemon local-embeddings")
	require.Contains(t, client.CredentialHealth().LastError, "kata daemon local-embeddings")
}

func TestLocalEmbeddingsSwitchRecipe(t *testing.T) {
	for _, tt := range []struct {
		name, recipe, modelEnv, dimsEnv, index string
		dims                                   int
		symlink                                bool
	}{
		{name: "Nomic", recipe: "model='nomic-embed-text'\ndims=768\ndocument_prefix='search_document: '\nquery_prefix='search_query: '\n", index: "kept", dims: 768},
		{name: "external revision", recipe: "model='nomic-embed-text'\ndims=768\nfingerprint_salt='changed-nomic-weights'\n", index: "rebuilt", dims: 768},
		{name: "temporary recipe override", recipe: "model='mxbai-embed-large'\ndims=1024\n", modelEnv: "nomic-embed-text", dimsEnv: "768", index: "kept", dims: 768},
		{name: "smaller width omitted", recipe: "model='nomic-embed-text'\ndims=512\n", index: "rebuilt", dims: 768},
		{name: "smaller width requested", recipe: "model='nomic-embed-text'\ndims=512\nrequest_dimensions=true\n", index: "kept", dims: 512},
		{name: "symlink", recipe: "model='nomic-embed-text'\ndims=768\n", index: "kept", dims: 768, symlink: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := setupKataEnv(t)
			stubLocalEmbeddingsInstall(t)
			t.Setenv("KATA_SERVER", "")
			t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", tt.modelEnv)
			t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", tt.dimsEnv)
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
			oldEndpoint := localEmbeddingsEndpoint
			localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
				require.True(t, opts.SkipOllama)
				return "http://127.0.0.1:12345/v1", nil
			}
			t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
			out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			require.NoError(t, err, out+stderr)
			require.Contains(t, stderr, "index will be "+tt.index)
			// Read again without transient shell settings to verify the persisted recipe.
			t.Setenv("KATA_SEARCH_EMBEDDINGS_MODEL", "")
			t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", "")
			next, err := config.ReadSearchConfig()
			require.NoError(t, err)
			require.True(t, next.Embeddings.UsesLocal())
			require.Empty(t, next.Embeddings.FingerprintSalt)
			require.Equal(t, tt.dims, next.Embeddings.Dims)
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0640), info.Mode().Perm())
			if tt.symlink {
				link, err := os.Readlink(path)
				require.NoError(t, err)
				require.Equal(t, "provider.toml", link)
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

func TestLocalEmbeddingsRestartRollback(t *testing.T) {
	for _, scenario := range []string{"existing symlink", "dangling symlink", "new", "concurrent edit"} {
		t.Run(scenario, func(t *testing.T) {
			home := setupKataEnv(t)
			stubLocalEmbeddingsInstall(t)
			t.Setenv("KATA_SERVER", "")
			path := filepath.Join(home, "config.toml")
			original := []byte("# original\n[search.embeddings]\nbase_url = 'http://127.0.0.1:11434/v1'\nmodel = 'nomic-embed-text'\n")
			if scenario != "new" {
				target := path
				if scenario == "existing symlink" || scenario == "dangling symlink" {
					target = filepath.Join(home, "provider.toml")
					if err := os.Symlink("provider.toml", path); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
				if scenario != "dangling symlink" {
					require.NoError(t, os.WriteFile(target, original, 0600))
				}
			}
			ns, err := daemonruntime.NewNamespace()
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:12345", Metadata: map[string]string{daemonruntime.RuntimeRestartExecutableKey: "1"}})
			require.NoError(t, err)
			oldEndpoint := localEmbeddingsEndpoint
			t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) { return "http://127.0.0.1:12345/v1", nil }
			oldExecutable := resolveDaemonExecutable
			t.Cleanup(func() { resolveDaemonExecutable = oldExecutable })
			edited := []byte("# edited while restart failed\n")
			resolveDaemonExecutable = func() (string, error) {
				if scenario == "concurrent edit" {
					require.NoError(t, os.WriteFile(path, edited, 0600))
				}
				return "", errors.New("replacement unavailable")
			}
			installed := false
			if scenario == "dangling symlink" {
				localEmbeddingsInstall = func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error) {
					installed = true
					return embedlocal.Installation{Executable: "/example/kennembed"}, nil
				}
			}
			out, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
			if scenario == "dangling symlink" {
				link, linkErr := os.Readlink(path)
				require.NoError(t, linkErr)
				require.Equal(t, "provider.toml", link)
				require.ErrorContains(t, err, "symlink")
				require.False(t, installed, "refuse before installing the shared server")
				_, err = os.Stat(filepath.Join(home, "provider.toml"))
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

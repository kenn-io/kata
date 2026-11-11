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
	got, err := applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	require.Equal(t, cfg.Embeddings, got)
	cfg, err = config.ParseSearchConfig([]byte("[search.embeddings]\ndims=1024\ndocument_prefix='document prefix'\ndocument_suffix='document suffix'\nquery_prefix='query prefix'\nquery_suffix='query suffix'\nfingerprint_salt='salt'\nrequest_dimensions=true\n"))
	require.NoError(t, err)
	got, err = applySharedEmbeddingChoice(t.Context(), cfg.Embeddings)
	require.NoError(t, err)
	require.Equal(t, config.EmbeddingsConfig{Local: true, Model: "nomic-embed-text", Dims: 768}, got)
}

func TestLocalEmbeddingsRestartRefusalPreservesChoice(t *testing.T) {
	for _, chosen := range []bool{false, true} {
		t.Run(fmt.Sprintf("chosen=%t", chosen), func(t *testing.T) {
			home := setupKataEnv(t)
			clearLocalEmbeddingsEnv(t)
			if chosen {
				require.NoError(t, embedlocal.RecordChoice())
			}
			stubLocalEmbeddingsInstall(t)
			oldEndpoint := localEmbeddingsEndpoint
			t.Cleanup(func() { localEmbeddingsEndpoint = oldEndpoint })
			localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
				if opts.Executable == "" {
					return "", embedlocal.ErrNotFound
				}
				return "http://127.0.0.1:12345/v1", nil
			}
			original := []byte("# original\n")
			path := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(path, original, 0600))
			_, err := enableLocalEmbeddings(t.Context(), localEmbeddingsRun{
				confirm: func(context.Context, int64) (bool, error) { return true, nil },
				notice:  func(string) error { return nil },
				restart: func(_ context.Context, apply func() error) error {
					require.NoError(t, apply())
					return errors.New("restart refused")
				},
			})
			require.ErrorContains(t, err, "restart refused")
			data, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration
			require.NoError(t, err)
			require.Equal(t, original, data)
			got, err := embedlocal.ChoiceRecorded()
			require.NoError(t, err)
			require.Equal(t, chosen, got)
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
	blocker := filepath.Join(tmp, ".kenn")
	require.NoError(t, os.WriteFile(blocker, []byte("block marker"), 0600))
	_, _, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.ErrorContains(t, err, "sharing could not be confirmed")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.Local)
	require.NoError(t, os.Remove(blocker))
	out, stderr, err := executeRootCapture(t, t.Context(), "daemon", "local-embeddings", "--yes")
	require.NoError(t, err, stderr)
	require.Contains(t, stderr, "290")
	require.Contains(t, out, "kata health --json")
	cfg, err = config.ReadSearchConfig()
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.Local)
	chosen, err := embedlocal.ChoiceRecorded()
	require.NoError(t, err)
	require.True(t, chosen)
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
	kataHome := setupKataEnv(t)
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
	chosen, err := embedlocal.ChoiceRecorded()
	require.NoError(t, err)
	require.False(t, chosen)
	run := localEmbeddingsRun{
		confirm: func(context.Context, int64) (bool, error) { return true, nil },
		notice:  func(string) error { return nil },
		restart: func(_ context.Context, apply func() error) error { return apply() },
	}
	_, err = enableLocalEmbeddings(t.Context(), run)
	require.ErrorContains(t, err, "sharing could not be confirmed")
	require.ErrorContains(t, err, "record search by meaning choice")
	cfg, err := config.ReadSearchConfig()
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.Local)
	client, _, err := preflightEmbeddingStartup(cfg.Embeddings, filepath.Join(kataHome, "kata.db"))
	require.NoError(t, err)
	require.NotNil(t, client)
	require.NoError(t, os.Remove(marker))
	_, err = enableLocalEmbeddings(t.Context(), run)
	require.NoError(t, err)
	chosen, err = embedlocal.ChoiceRecorded()
	require.NoError(t, err)
	require.True(t, chosen)
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
		chosen   bool
	}{
		{name: "edited", exists: true, original: "[daemon]\nport = 8888\n", edited: "[daemon]\nport = 9999\n"},
		{name: "created empty"},
		{name: "edited with existing choice", exists: true, original: "[daemon]\nport = 8888\n", edited: "[daemon]\nport = 9999\n", chosen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupKataEnv(t)
			if tc.chosen {
				require.NoError(t, embedlocal.RecordChoice())
			}
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
			chosen, err := embedlocal.ChoiceRecorded()
			require.NoError(t, err)
			require.Equal(t, tc.chosen, chosen)
		})
	}
}

func TestLocalEmbeddingsDaemonDownloadConsent(t *testing.T) {
	for _, tc := range []struct {
		name              string
		chosen, installed bool
	}{
		{name: "no choice"}, {name: "recorded choice", chosen: true}, {name: "installed", installed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearLocalEmbeddingsEnv(t)
			home := setupKataEnv(t)
			if tc.chosen {
				require.NoError(t, embedlocal.RecordChoice())
			}
			var endpoint string
			if tc.installed {
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					values := make([]float64, 768)
					values[0] = 1
					if err := json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"embedding": values, "index": 0}}}); err != nil {
						t.Errorf("encode embedding response: %v", err)
					}
				}))
				t.Cleanup(provider.Close)
				endpoint = provider.URL
			}
			oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
			t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
			localEmbeddingsEndpoint = func(_ context.Context, opts embedlocal.Options) (string, error) {
				if tc.installed && opts.Executable != "" {
					return endpoint, nil
				}
				return "", embedlocal.ErrNotFound
			}
			localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				if tc.installed {
					return embedlocal.Installation{Executable: "/example/kennembed"}, nil
				}
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
			switch {
			case tc.installed:
				require.NoError(t, err)
				chosen, err := embedlocal.ChoiceRecorded()
				require.NoError(t, err)
				require.False(t, chosen)
			case tc.chosen:
				require.ErrorContains(t, err, "installation reached")
			default:
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

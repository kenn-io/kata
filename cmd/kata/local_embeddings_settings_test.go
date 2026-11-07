package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kit/embedlocal"
)

func TestLocalEmbeddingsSetupTurnOn(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "restart", true: "refusal restores config"}[refused], func(t *testing.T) {
			clearLocalEmbeddingsEnv(t)
			home := setupKataEnv(t)
			original := []byte("# original\n")
			path := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(path, original, 0600))
			oldEndpoint, oldInstall := localEmbeddingsEndpoint, localEmbeddingsInstall
			t.Cleanup(func() { localEmbeddingsEndpoint, localEmbeddingsInstall = oldEndpoint, oldInstall })
			installed := false
			localEmbeddingsEndpoint = func(context.Context, embedlocal.Options) (string, error) {
				if !installed {
					return "", embedlocal.ErrNotFound
				}
				return "http://127.0.0.1:12345/v1", nil
			}
			var run func(context.Context)
			restarts := 0
			setup := &localEmbeddingsSetup{start: func(work func(context.Context)) bool { run = work; return true }, restart: func(_ context.Context) error {
				restarts++
				if refused {
					return errors.New("restart refused")
				}
				return nil
			}}
			localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
				yes, err := opts.Confirm(ctx, 290_000_000)
				require.NoError(t, err)
				require.True(t, yes)
				opts.Progress(120, 290)
				status, err := setup.Status(ctx)
				require.NoError(t, err)
				require.Equal(t, int64(120), status.Job.Downloaded)
				installed = true
				return embedlocal.Installation{Executable: "/example/kennembed"}, nil
			}
			require.NoError(t, setup.TurnOn(t.Context()))
			require.ErrorIs(t, setup.TurnOn(t.Context()), daemon.ErrLocalEmbeddingsBusy)
			run(t.Context())
			require.Equal(t, 1, restarts)
			got, err := os.ReadFile(path) // #nosec G304 -- configuration in the test home
			require.NoError(t, err)
			status, err := setup.Status(t.Context())
			require.NoError(t, err)
			if refused {
				require.Equal(t, original, got)
				require.Equal(t, "failed", status.Job.State)
				require.Contains(t, status.Job.Error, "restart refused")
			} else {
				require.Contains(t, string(got), "local = true")
				require.Equal(t, "restarting", status.Job.State)
			}
		})
	}
}

func TestLocalEmbeddingsSetupTurnOnBuiltIn(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	source := []byte("[search.embeddings]\nlocal = true\n")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, source, 0600))
	cfg, err := config.ParseSearchConfig(source)
	require.NoError(t, err)
	require.True(t, cfg.Embeddings.UsesLocal())
	setup := &localEmbeddingsSetup{embeddings: cfg.Embeddings, start: func(func(context.Context)) bool {
		t.Fatal("setup job started while built-in")
		return false
	}, restart: func(context.Context) error {
		t.Fatal("daemon restarted while built-in")
		return nil
	}}
	require.NoError(t, setup.TurnOn(t.Context()))
}

func TestLocalEmbeddingsSetupStatus(t *testing.T) {
	clearLocalEmbeddingsEnv(t)
	home := setupKataEnv(t)
	source := []byte("[search.embeddings]\nbase_url = \"http://127.0.0.1:11434/v1\"\nmodel = \"mxbai-embed-large\"\ndims = 1024\n")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), source, 0600))
	cfg, err := config.ParseSearchConfig(source)
	require.NoError(t, err)
	s := &localEmbeddingsSetup{embeddings: cfg.Embeddings}
	status, err := s.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ollama", status.Provider)
	require.True(t, status.Rebuild)
	require.False(t, status.CanChange)
}

//go:build kata_browser_fixture

package main

import (
	"context"
	"errors"
	"os"

	"go.kenn.io/kit/embedlocal"
)

// Browser fixtures must not discover a service running on the host.
func init() {
	localEmbeddingsInspect = func(ctx context.Context, opts embedlocal.Options) (embedlocal.Status, error) {
		opts.SkipOllama = true
		return embedlocal.Inspect(ctx, opts)
	}
	localEmbeddingsEndpoint = func(ctx context.Context, opts embedlocal.Options) (string, error) {
		path := os.Getenv("KATA_BROWSER_EMBEDDINGS_FIXTURE")
		if path != "" {
			endpoint, err := os.ReadFile(path)
			if err == nil {
				if _, err := os.Stat(path + ".installed"); err != nil {
					return "", embedlocal.ErrNotFound
				}
				return string(endpoint), nil
			}
		}
		opts.SkipOllama = true
		return embedlocal.Endpoint(ctx, opts)
	}
	localEmbeddingsInstall = func(ctx context.Context, opts embedlocal.InstallOptions) (embedlocal.Installation, error) {
		path := os.Getenv("KATA_BROWSER_EMBEDDINGS_FIXTURE")
		if _, err := os.Stat(path); path == "" || err != nil {
			return embedlocal.Installation{}, errors.New("browser embedding fixture unavailable")
		}
		confirmed, err := opts.Confirm(ctx, 1)
		if err != nil {
			return embedlocal.Installation{}, err
		}
		if !confirmed {
			return embedlocal.Installation{}, errors.New("embedding installation declined")
		}
		if err := os.WriteFile(path+".installed", nil, 0o600); err != nil {
			return embedlocal.Installation{}, err
		}
		return embedlocal.Installation{Executable: path + ".installed", DataDir: opts.DataDir}, nil
	}
}

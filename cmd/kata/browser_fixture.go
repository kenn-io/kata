//go:build kata_browser_fixture

package main

import (
	"context"

	"go.kenn.io/kit/embedlocal"
)

// Browser fixtures must not discover a service running on the host.
func init() {
	localEmbeddingsInspect = func(ctx context.Context, opts embedlocal.Options) (embedlocal.Status, error) {
		opts.SkipOllama = true
		return embedlocal.Inspect(ctx, opts)
	}
	localEmbeddingsEndpoint = func(ctx context.Context, opts embedlocal.Options) (string, error) {
		opts.SkipOllama = true
		return embedlocal.Endpoint(ctx, opts)
	}
}

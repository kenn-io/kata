package embedding

import (
	"context"
	"errors"

	"go.kenn.io/kit/embedlocal"
)

// LocalEndpoint discovers the shared server or starts it from installed assets.
func LocalEndpoint(ctx context.Context) (string, error) {
	dir, err := embedlocal.DefaultDir()
	if err != nil {
		return "", err
	}
	return localEndpoint(ctx, dir, embedlocal.Endpoint, embedlocal.Install)
}

func localEndpoint(ctx context.Context, dir string, endpoint func(context.Context, embedlocal.Options) (string, error), install func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error)) (string, error) {
	opts := embedlocal.Options{DataDir: dir, SkipOllama: true}
	url, err := endpoint(ctx, opts)
	if !errors.Is(err, embedlocal.ErrNotFound) {
		return url, err
	}
	installed, err := install(ctx, embedlocal.InstallOptions{DataDir: dir})
	if err != nil {
		return "", err
	}
	opts.Executable = installed.Executable
	return endpoint(ctx, opts)
}

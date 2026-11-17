package embedding

import (
	"context"
	"errors"

	"go.kenn.io/kit/embedlocal"
)

// ResolveLocal finds the shared server or starts it from installed assets.
// The caller supplies confirmation before kit downloads any missing assets.
func ResolveLocal(ctx context.Context, endpoint func(context.Context, embedlocal.Options) (string, error), install func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error), confirm func(context.Context, int64) (bool, error)) (string, error) {
	url, err := endpoint(ctx, embedlocal.Options{SkipOllama: true})
	if !errors.Is(err, embedlocal.ErrNotFound) {
		return url, err
	}
	dir, err := embedlocal.DefaultDir()
	if err != nil {
		return "", err
	}
	installed, err := install(ctx, embedlocal.InstallOptions{DataDir: dir, Confirm: confirm})
	if err != nil {
		return "", err
	}
	return endpoint(ctx, embedlocal.Options{SkipOllama: true, Executable: installed.Executable})
}

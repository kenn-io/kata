package embedding

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedlocal"
)

func TestResolveLocalDoesNotDownloadMissingAssets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".kenn", "embeddings"), 0o700))
	got, err := ResolveLocal(t.Context(), func(context.Context, embedlocal.Options) (string, error) { return "", embedlocal.ErrNotFound }, embedlocal.Install, nil)
	require.Empty(t, got)
	require.Error(t, err)
	require.False(t, errors.Is(err, embedlocal.ErrNotFound))
}

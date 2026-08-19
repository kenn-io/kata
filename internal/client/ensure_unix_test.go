//go:build !windows

package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestAutoStartRejectsSymlinkStateDirectoryBeforeSpawn(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	require.NoError(t, os.Mkdir(target, 0o700))
	dataDir := filepath.Join(base, "runtime")
	require.NoError(t, os.Symlink(target, dataDir))

	originalStart := startDetachedDaemonForEnsure
	spawnErr := errors.New("unexpected daemon spawn")
	started := false
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
		started = true
		return spawnErr
	}
	t.Cleanup(func() { startDetachedDaemonForEnsure = originalStart })

	_, err := autoStart(t.Context(), dataDir)

	require.Error(t, err)
	assert.False(t, started, "daemon should not be spawned for a symlinked state directory")
	assert.NotErrorIs(t, err, spawnErr)
}

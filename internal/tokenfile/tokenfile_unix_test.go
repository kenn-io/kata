//go:build !windows

package tokenfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReserveUnixRequiresPrivateDirectoryAndExclusiveDestination(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // Directory traversal requires the owner's execute bit.
	path := filepath.Join(dir, "worker.token")
	reservation, err := Reserve(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reservation.Abort() })
	require.NoError(t, reservation.Commit("test-token"))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	_, err = Reserve(path)
	require.ErrorContains(t, err, "already exists")
	contents, err := os.ReadFile(path) //nolint:gosec // Test-owned credential path under t.TempDir.
	require.NoError(t, err)
	require.Equal(t, "test-token\n", string(contents))

	link := filepath.Join(dir, "linked.token")
	require.NoError(t, os.Symlink(path, link))
	_, err = Reserve(link)
	require.ErrorContains(t, err, "already exists")
	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // Exercise rejection of a non-private directory.
	_, err = Reserve(filepath.Join(dir, "public.token"))
	require.ErrorContains(t, err, "must be owner-only")
}

func TestReservationFailedDeliveryRemovesUnixFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // Directory traversal requires the owner's execute bit.
	path := filepath.Join(dir, "worker.token")
	reservation, err := Reserve(path)
	require.NoError(t, err)
	// A failed write must not leave a partial credential at the delivery path.
	require.NoError(t, reservation.file.Close())
	require.ErrorContains(t, reservation.Commit("test-token"), "write token file")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, reservation.Abort())
}

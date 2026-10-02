package jsonl_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// Contract vmr7: migration must preserve the canonical path protected by the
// daemon lock, including when an operator configured a symlink to the database.
func TestAutoCutoverPreservesSymlinkDatabaseLockIdentity(t *testing.T) {
	target := filepath.Join(t.TempDir(), "kata.db")
	writeLegacyV1DB(t, target)
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	release, err := daemon.AcquireDatabaseLock(alias)
	require.NoError(t, err)
	defer release()
	require.NoError(t, jsonl.AutoCutover(t.Context(), alias))

	secondRelease, err := daemon.AcquireDatabaseLock(alias)
	if secondRelease != nil {
		defer secondRelease()
	}
	require.ErrorContains(t, err, "daemon already running")
	info, err := os.Lstat(alias)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	version, err := sqlitestore.PeekSchemaVersion(t.Context(), target)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
}

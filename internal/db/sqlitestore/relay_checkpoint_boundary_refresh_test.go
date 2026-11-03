package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRelayRootCheckpointLaterBoundary(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "root.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRootCheckpointLaterBoundary(t, store, func(t *testing.T) db.Storage {
		t.Helper()
		replica, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "relay.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = replica.Close() })
		return replica
	})
}

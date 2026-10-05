package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRelayLegacyCachedResetPrivacy(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "legacy.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayCrossProjectBoundary(t, store, func(ctx context.Context, key, value string) error {
		_, err := store.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=?`, value, key)
		return err
	})
}

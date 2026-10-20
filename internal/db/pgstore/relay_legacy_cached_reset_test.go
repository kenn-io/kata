package pgstore_test

import (
	"context"
	"testing"

	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestRelayLegacyCachedResetPrivacy(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayLegacyCachedResetPrivacy(t, store, func(ctx context.Context, key, value string) error {
		_, err := store.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, value, key)
		return err
	})
}

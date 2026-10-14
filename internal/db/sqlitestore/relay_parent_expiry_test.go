package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRelayParentCredentialCanExpire(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "parent.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayParentCredentialExpiration(t, store)
}

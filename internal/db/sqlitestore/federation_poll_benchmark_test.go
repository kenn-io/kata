package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

func BenchmarkFederationPoll(b *testing.B) {
	b.Cleanup(sqlitestore.CountStatementsForTest())
	store, err := sqlitestore.Open(b.Context(), filepath.Join(b.TempDir(), "kata.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	testenv.BenchmarkFederationPoll(b, store)
}

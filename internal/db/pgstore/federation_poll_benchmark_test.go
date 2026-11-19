package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func BenchmarkFederationPoll(b *testing.B) {
	dsn, cleanup := conformanceTemplate.NewDatabase(b, b.Context())
	b.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(b.Context(), dsn, conformanceConfig())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	restore, err := pgstore.CountStatementsForTest(store)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, restore()) })
	testenv.BenchmarkFederationPoll(b, store)
}

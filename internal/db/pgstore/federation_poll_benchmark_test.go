package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func BenchmarkFederationPoll(b *testing.B) {
	b.Cleanup(pgstore.CountStatementsForTest())
	dsn, cleanup := conformanceTemplate.NewDatabase(b, b.Context())
	b.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(b.Context(), dsn, conformanceConfig())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	testenv.BenchmarkFederationPoll(b, store)
}

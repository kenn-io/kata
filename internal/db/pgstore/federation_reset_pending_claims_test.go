package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestResetFederatedProjectIgnoresRejectedPendingClaims(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunFederationResetIgnoresRejectedPendingClaims(t, store)
}

package pgstore_test

import (
	"testing"

	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestRootNativeAttribution(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootNativeAttribution(t, s)
}

func TestAttributionReceiptBeforeEvent(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunAttributionReceiptBeforeEvent(t, s)
}

func TestRootKeyRotationReplicaAuthority(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootKeyRotationReplicaAuthority(t, s)
}

func TestRootSigningKeyUnavailable(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootSigningKeyUnavailable(t, s)
}

func TestRootKeyRepin(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootKeyRepin(t, s)
}

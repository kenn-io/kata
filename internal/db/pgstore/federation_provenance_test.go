package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRelayAttribution(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	s, e := pgstore.Open(t.Context(), dsn)
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	dbtest.RunRelayAttribution(t, s)
	dbtest.RunUpstreamAttribution(t, s)
}

func TestRootNativeAttribution(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootNativeAttribution(t, s)
}

func TestAttributionReceiptBeforeEvent(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunAttributionReceiptBeforeEvent(t, s)
}

func TestRootKeyRotation(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootKeyRotation(t, s)
}

func TestRootKeyRotationReplicaAuthority(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootKeyRotationReplicaAuthority(t, s)
}

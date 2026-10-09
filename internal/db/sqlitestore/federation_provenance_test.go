package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRelayAttribution(t *testing.T) {
	s, e := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	dbtest.RunRelayAttribution(t, s)
	dbtest.RunUpstreamAttribution(t, s)
}

func TestRootNativeAttribution(t *testing.T) {
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootNativeAttribution(t, s)
}

func TestAttributionReceiptBeforeEvent(t *testing.T) {
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "receipt-first.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunAttributionReceiptBeforeEvent(t, s)
}

func TestRootKeyRotation(t *testing.T) {
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "native.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootKeyRotation(t, s)
}

func TestRootKeyRotationReplicaAuthority(t *testing.T) {
	s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRootKeyRotationReplicaAuthority(t, s)
}

package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRelayOutboxAtomicity(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "relay.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayOutboxAtomicity(t, store)
}

func TestRelayOfflineIntent(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayOfflineIntent(t, store)
}

func TestRelayRootAcceptanceStatus(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRootAcceptanceStatus(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, projectID)
		return err
	})
}

func TestSignedRelayResetCommitments(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "reset.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunSignedRelayResetCommitments(t, store)
}

func TestCreateRelayResetAfterCompaction(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunCreateRelayResetAfterCompaction(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, projectID)
		return err
	})
}

func TestForwardCompactedRelayCheckpointPostCheckpointIngress(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunForwardCompactedRelayCheckpointPostCheckpointIngress(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, projectID)
		return err
	})
}

func TestRelayRevokedOutboxLifecycle(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRevokedOutboxLifecycle(t, store)
}

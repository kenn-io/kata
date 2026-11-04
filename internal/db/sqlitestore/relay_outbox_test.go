package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRelayOfflineIntent(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayOfflineIntent(t, store)
	dbtest.RunRelayReconnectOfflineIntent(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, projectID)
		return err
	})
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

func TestRelayResetForwardsToActiveDownstream(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetForwardsToActiveDownstream(t, store)
}

func TestRelayResetRepeatedBeforeDownstreamActivation(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetRepeatedBeforeDownstreamActivation(t, store)
}

func TestRelayResetSupersedesArtifactOffers(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetSupersedesArtifactOffers(t, store)
}

func TestRelayResetRefreshSupersedesArtifactOffers(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetRefreshSupersedesArtifactOffers(t, store)
}

func TestRelayRevokedOutboxLifecycle(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRevokedOutboxLifecycle(t, store)
}

func TestRelayResetCarriesCronDefinitions(t *testing.T) {
	root, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "root.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	dbtest.RunRelayResetCarriesCronDefinitions(t, root, func(t *testing.T) db.Storage {
		t.Helper()
		downstream, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "downstream.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = downstream.Close() })
		return downstream
	})
}

func TestRelayLocalDetach(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayLocalDetach(t, store)
}

func TestRelayRevocationPrunesBindingState(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRevocationPrunesBindingState(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, projectID)
		return err
	})
}

func TestRelayResetPreparesForSuspendedDownstream(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetPreparesForSuspendedDownstream(t, store, func(ctx context.Context, tokenID int64, expires time.Time) error {
		_, err := store.ExecContext(ctx, `UPDATE api_tokens SET expires_at=? WHERE id=?`, expires.UTC().Format(db.EventTimestampFormat), tokenID)
		return err
	})
}

package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRelayOfflineIntent(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayOfflineIntent(t, store)
	dbtest.RunRelayReconnectOfflineIntent(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID)
		return err
	})
}

func TestRelayRootAcceptanceStatus(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRootAcceptanceStatus(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID)
		return err
	})
}

func TestSignedRelayResetCommitments(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunSignedRelayResetCommitments(t, store)
}

func TestCreateRelayResetAfterCompaction(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunCreateRelayResetAfterCompaction(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID)
		return err
	})
}

func TestForwardCompactedRelayCheckpointPostCheckpointIngress(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunForwardCompactedRelayCheckpointPostCheckpointIngress(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID)
		return err
	})
}

func TestRelayResetForwardsToActiveDownstream(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetForwardsToActiveDownstream(t, store)
}

func TestRelayResetRepeatedBeforeDownstreamActivation(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetRepeatedBeforeDownstreamActivation(t, store)
}

func TestRelayResetSupersedesArtifactOffers(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetSupersedesArtifactOffers(t, store)
}

func TestRelayResetRefreshSupersedesArtifactOffers(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetRefreshSupersedesArtifactOffers(t, store)
}

func TestRelayRevokedOutboxLifecycle(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRevokedOutboxLifecycle(t, store)
}

func TestRelayResetCarriesCronDefinitions(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	root, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	dbtest.RunRelayResetCarriesCronDefinitions(t, root, func(t *testing.T) db.Storage {
		t.Helper()
		downstream, err := pgstore.OpenWithConfig(t.Context(), dsn, pgstore.Config{Schema: "relay_downstream", SchemaMode: pgstore.SchemaModeBootstrap})
		require.NoError(t, err)
		t.Cleanup(func() { _ = downstream.Close() })
		return downstream
	})
}

func TestRelayLocalDetach(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayLocalDetach(t, store)
}

func TestRelayRevocationPrunesBindingState(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRevocationPrunesBindingState(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID)
		return err
	})
}

func TestRelayResetPreparesForSuspendedDownstream(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayResetPreparesForSuspendedDownstream(t, store, func(ctx context.Context, tokenID int64, expires time.Time) error {
		_, err := store.ExecContext(ctx, `UPDATE api_tokens SET expires_at=$1 WHERE id=$2`, expires.UTC().Format(db.EventTimestampFormat), tokenID)
		return err
	})
}

package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestEmbeddingArtifactStorage(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "artifacts.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunEmbeddingArtifactStorage(t, store)
}

func TestArtifactStagingExpiryRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staging.db")
	store, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactStagingExpiryRetry(t, store, func() db.Storage {
		require.NoError(t, store.Close())
		next, err := sqlitestore.Open(t.Context(), path)
		require.NoError(t, err)
		store = next
		return next
	}, func(ctx context.Context, projectUID string) {
		_, err := store.ExecContext(ctx, `UPDATE federation_embedding_artifacts SET staging_expires_at='2000-01-01T00:00:00Z' WHERE project_uid=? AND staging_expires_at IS NOT NULL`, projectUID)
		require.NoError(t, err)
	})
}

func TestArtifactStagingByteBudget(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "bytes.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactStagingByteBudget(t, store)
}

func TestArtifactRelayOutbox(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "outbox.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactRelayOutbox(t, store)
}

func TestArtifactManifestAcknowledgedPrefix(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "manifest.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactManifestAcknowledgedPrefix(t, store)
}

func TestArtifactManifestStaging(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "manifest-stage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactManifestStaging(t, store)
}

func TestArtifactManifestDownload(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "manifest-download.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactManifestDownload(t, store)
}

func TestArtifactManifestOfferBounds(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "manifest-bounds.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactManifestOfferBounds(t, store)
}

func TestArtifactManifestBootstrap(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "manifest-bootstrap.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactManifestBootstrap(t, store)
}

func TestArtifactManifestOutgoingBootstrap(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "manifest-outgoing.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactManifestOutgoingBootstrap(t, store)
}

func TestArtifactSignedResetManifests(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "manifest-reset.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactSignedResetManifests(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=?`, projectID)
		return err
	})
}

func TestArtifactResetInstallation(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "artifact-reset-install.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactResetInstallation(t, store)
}

func TestArtifactPopulatedAdoption(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "artifact-adoption.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactPopulatedAdoption(t, store)
}

func TestArtifactIssuePurge(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "artifact-purge.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunArtifactIssuePurge(t, store)
}

func TestEmbeddingProducerRootOwnership(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "producer-authority.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunEmbeddingProducerRootOwnership(t, store)
}
func TestEmbeddingProducerIngressOwnership(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "producer-authority.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunEmbeddingProducerIngressOwnership(t, store)
}
func TestDownstreamCannotForgeRootEmbeddingProducer(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "producer-forgery.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunDownstreamCannotForgeRootEmbeddingProducer(t, store)
}
func TestEmbeddingProducerRootPropagation(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "producer-authority.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunEmbeddingProducerRootPropagation(t, store)
}
func TestEmbeddingProducerConfigurationValidation(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "producer-authority.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunEmbeddingProducerConfigurationValidation(t, store)
}

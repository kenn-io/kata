package pgstore_test

import (
	"context"
	"testing"

	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestRelaySharedAccessEnrollment(t *testing.T) {
	for _, form := range []string{"enrollment", "to", "from", "snapshot", "no-ref-omitted", "no-ref-empty", "no-ref-null", "moved-reply-purge", "moved-bare-live", "moved-bare-purge", "moved-bare-reuse", "moved-bare-reuse-purge", "moved-bare-return"} {
		t.Run(form, func(t *testing.T) {
			dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
			t.Cleanup(cleanup)
			store, err := pgstore.Open(t.Context(), dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			compact := func(ctx context.Context, projectID int64) error {
				_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID)
				return err
			}
			switch form {
			case "enrollment":
				dbtest.RunRelaySharedAccessEnrollment(t, store, compact)
			case "moved-reply-purge":
				dbtest.RunRelaySharedAccessMovedReplyPurge(t, store)
			case "moved-bare-live", "moved-bare-purge":
				dbtest.RunRelaySharedAccessMovedBareEvidence(t, store, form == "moved-bare-purge")
			case "moved-bare-reuse":
				dbtest.RunRelaySharedAccessMovedBareEvidenceReusedShortID(t, store)
			case "moved-bare-reuse-purge":
				dbtest.RunRelaySharedAccessMovedBareEvidenceReusedShortIDAfterPurge(t, store)
			case "moved-bare-return":
				dbtest.RunRelaySharedAccessMovedBareEvidenceReturnedToSource(t, store)
			default:
				dbtest.RunRelaySharedAccessNotification(t, store, form, compact)
			}
		})
	}
}

func TestRelayRootAcceptanceParent(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRootAcceptanceParent(t, store)
}

func TestRelayFreshTargetPolicyEpoch(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayFreshTargetPolicyEpoch(t, store)
}

func TestRelayLegacyGrantIsolation(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayLegacyGrantIsolation(t, store)
}

func TestRelayIngressClaimLifecycle(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayIngressClaimLifecycle(t, store)
}

func TestRelayDownstreamOrigin(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayDownstreamOrigin(t, store)
}

func TestRelayIngestValidatesCronReplay(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayIngestValidatesCronReplay(t, store)
}

func TestRelayUpstreamLocalAuthority(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayUpstreamLocalAuthority(t, store)
}

func TestRelayAuthorityDepth(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayAuthorityDepth(t, store)
}

func TestRelayEnrollmentBootstrap(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	s, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	dbtest.RunRelayEnrollmentBootstrap(t, s)
}

func TestRelayEnrollmentBootstrapCompactedArtifacts(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayEnrollmentBootstrapCompactedArtifacts(t, store, func(ctx context.Context, projectID int64) error {
		_, err := store.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1`, projectID)
		return err
	})
}

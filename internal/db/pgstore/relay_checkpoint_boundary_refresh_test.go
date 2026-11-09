package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

func TestRelayRootCheckpointLaterBoundary(t *testing.T) {
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRootCheckpointLaterBoundary(t, store, func(t *testing.T) db.Storage {
		t.Helper()
		dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
		t.Cleanup(cleanup)
		replica, err := pgstore.Open(t.Context(), dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = replica.Close() })
		return replica
	}, func(t *testing.T, project db.Project, event db.RemoteEvent) {
		t.Helper()
		_, err := store.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			event.EventUID, event.OriginInstanceUID, project.ID, project.Name, *event.IssueUID, event.Type, event.Actor,
			string(event.Payload), event.HLCPhysicalMS, event.HLCCounter, event.ContentHash, event.CreatedAt.Format("2006-01-02T15:04:05.000Z"))
		require.NoError(t, err)
		require.NoError(t, store.MaterializeFederatedProject(t.Context(), project.ID))
	})
}

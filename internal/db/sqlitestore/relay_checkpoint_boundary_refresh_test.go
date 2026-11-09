package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRelayRootCheckpointLaterBoundary(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "root.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	dbtest.RunRelayRootCheckpointLaterBoundary(t, store, func(t *testing.T) db.Storage {
		t.Helper()
		replica, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "relay.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = replica.Close() })
		return replica
	}, func(t *testing.T, project db.Project, event db.RemoteEvent) {
		t.Helper()
		_, err := store.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			event.EventUID, event.OriginInstanceUID, project.ID, project.Name, *event.IssueUID, event.Type, event.Actor,
			string(event.Payload), event.HLCPhysicalMS, event.HLCCounter, event.ContentHash, event.CreatedAt.Format("2006-01-02T15:04:05.000Z"))
		require.NoError(t, err)
		require.NoError(t, store.MaterializeFederatedProject(t.Context(), project.ID))
	})
}

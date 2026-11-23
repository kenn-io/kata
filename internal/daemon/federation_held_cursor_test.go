package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestEnableReplicaPushResumesHeldCursor(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "held-project")
	require.NoError(t, err)
	_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "offline work", Author: "worker"})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true})
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `INSERT INTO federation_held_cursors(project_uid,push_cursor_event_id) VALUES(?,0)`, project.UID)
	require.NoError(t, err)
	binding, err := enableReplicaPush(ctx, store, project.ID)
	require.NoError(t, err)
	require.Zero(t, binding.PushCursorEventID)
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, event.UID, pending[0].UID)
}

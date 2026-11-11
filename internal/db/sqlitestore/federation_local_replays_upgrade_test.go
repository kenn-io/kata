package sqlitestore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testfix"
)

func TestFederationLocalReplaysUpgradeBackfillsInterruptedReset(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, err := testfix.OpenSQLite(ctx, path)
	require.NoError(t, err)
	resetProject, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	pulledProject, err := store.CreateProject(ctx, "pulled-project")
	require.NoError(t, err)
	events := map[string]string{}
	var acknowledgedID int64
	for _, c := range []struct {
		name    string
		project db.Project
	}{{"unprojected", resetProject}, {"acknowledged", resetProject}, {"before", resetProject}, {"after", resetProject}, {"pulled", pulledProject}} {
		_, event, createErr := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: c.project.ID, Title: c.name, Author: "tester"})
		require.NoError(t, createErr)
		events[c.name] = event.UID
		if c.name == "acknowledged" {
			acknowledgedID = event.ID
		}
	}
	for _, project := range []db.Project{resetProject, pulledProject} {
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:1",
			HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: 51,
			PullCursorEventID: 50, Actor: "tester", Enabled: true,
		})
		require.NoError(t, err)
		require.NoError(t, store.RecordFederationSyncReset(ctx, project.ID, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)))
	}
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, resetProject.ID, acknowledgedID))
	stamp := func(name, createdAt string) {
		_, execErr := store.ExecContext(ctx, `UPDATE events SET created_at=? WHERE uid=?`, createdAt, events[name])
		require.NoError(t, execErr)
	}
	stamp("before", "2026-01-01T00:00:00.000Z")
	stamp("unprojected", "2026-01-01T00:00:00.000Z")
	stamp("acknowledged", "2026-01-01T00:00:00.000Z")
	stamp("after", "2026-01-03T00:00:00.000Z")
	stamp("pulled", "2026-01-01T00:00:00.000Z")
	_, err = store.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE uid=?`, events["unprojected"])
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `DELETE FROM issues WHERE uid=(SELECT issue_uid FROM events WHERE uid=?)`, events["unprojected"])
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `UPDATE federation_sync_status SET last_pull_success_at='2026-01-02T12:00:00.000Z' WHERE project_id=?`, pulledProject.ID)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `DROP TABLE federation_local_replays; DROP TABLE federation_push_accepted`)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `UPDATE meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	require.NoError(t, sqlitestore.UpgradeInPlace(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	for name, want := range map[string]bool{"unprojected": true, "acknowledged": false, "before": true, "after": false, "pulled": false} {
		got, _, replayErr := upgraded.FederationLocalReplayStatus(ctx, events[name])
		require.NoError(t, replayErr)
		require.Equal(t, want, got, name)
	}
}

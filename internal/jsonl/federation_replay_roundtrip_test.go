package jsonl_test

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestFederationReplayJSONLRoundTrip(t *testing.T) {
	ctx := t.Context()
	source, events := federationReplayJSONLFixture(t, true)
	backup := exportToBuffer(ctx, t, source).Bytes()

	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup), restored))
	requireFederationReplayJSONL(t, restored, events)

	var scoped bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &scoped, jsonl.ExportOptions{ProjectID: events[0].ProjectID}))
	scopedRestore := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &scoped, scopedRestore))
	_, err := scopedRestore.EventsByUIDs(ctx, events[0].ProjectID, []string{events[0].UID})
	require.NoError(t, err, "the scoped export still carries event history")
	due, err := scopedRestore.FederationLocalReplayDue(ctx, events[0].UID, true)
	require.NoError(t, err)
	require.False(t, due, "project exports omit replay delivery state")

	newInstance := openImportTargetDB(t)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(backup), newInstance, jsonl.ImportOptions{NewInstance: true}))
	due, err = newInstance.FederationLocalReplayDue(ctx, events[0].UID, true)
	require.NoError(t, err)
	require.False(t, due, "a new instance does not inherit local replay markers")
}

func TestFederationReplayJSONLRoundTripAcrossPostgres(t *testing.T) {
	postgres := openPostgresImportTarget(t)
	ctx := t.Context()
	source, events := federationReplayJSONLFixture(t, true)
	backup := exportToBuffer(ctx, t, source)
	require.NoError(t, jsonl.Import(ctx, backup, postgres))

	// Export before probing the quiet marker, which records delivery debt.
	var back bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, postgres, &back, jsonl.ExportOptions{IncludeDeleted: true}))
	requireFederationReplayJSONL(t, postgres, events)

	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, &back, restored))
	requireFederationReplayJSONL(t, restored, events)
}

func federationReplayJSONLFixture(t *testing.T, withMarkers bool) (*sqlitestore.Store, []db.Event) {
	t.Helper()
	ctx := t.Context()
	source := openImportTargetDB(t)
	project, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	var events []db.Event
	for _, title := range []string{"Pending replay", "Acknowledged replay", "Ordinary local event"} {
		_, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "tester"})
		require.NoError(t, err)
		events = append(events, event)
	}
	if withMarkers {
		_, err = source.ExecContext(ctx, `INSERT INTO federation_local_replays(event_uid, delivery_due) VALUES (?, 1), (?, 0)`, events[0].UID, events[1].UID)
		require.NoError(t, err)
	}
	return source, events
}

func requireFederationReplayJSONL(t *testing.T, store db.Storage, events []db.Event) {
	t.Helper()
	ctx := t.Context()
	due, err := store.FederationLocalReplayDue(ctx, events[0].UID, false)
	require.NoError(t, err)
	require.True(t, due, "a full restore preserves pending replay delivery")
	due, err = store.FederationLocalReplayDue(ctx, events[1].UID, false)
	require.NoError(t, err)
	require.False(t, due, "an acknowledged replay stays silent")
	due, err = store.FederationLocalReplayDue(ctx, events[1].UID, true)
	require.NoError(t, err)
	require.True(t, due, "the acknowledged replay marker still exists")
	due, err = store.FederationLocalReplayDue(ctx, events[2].UID, true)
	require.NoError(t, err)
	require.False(t, due, "ordinary history must not become a replay")
}

func TestFederationReplayLegacyJSONLBackupRecoversInterruptedReset(t *testing.T) {
	ctx := t.Context()
	source, events := federationReplayJSONLFixture(t, false)
	project, err := source.ProjectByID(ctx, events[0].ProjectID)
	require.NoError(t, err)
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:1",
		HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: 51,
		PullCursorEventID: 50, PushCursorEventID: events[1].ID, Actor: "tester", Enabled: true,
	})
	require.NoError(t, err)
	resetAt := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	require.NoError(t, source.RecordFederationSyncReset(ctx, project.ID, resetAt))
	_, err = source.ExecContext(ctx, `UPDATE events SET created_at=? WHERE uid IN (?,?)`,
		resetAt.Add(-time.Hour).Format(db.EventTimestampFormat), events[0].UID, events[1].UID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE events SET created_at=? WHERE uid=?`,
		resetAt.Add(time.Hour).Format(db.EventTimestampFormat), events[2].UID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE uid=?`, events[0].UID)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `DELETE FROM issues WHERE uid=?`, *events[0].IssueUID)
	require.NoError(t, err)

	current := exportToBuffer(ctx, t, source).Bytes()
	version := []byte(`"key":"export_version","value":"` + strconv.Itoa(db.CurrentSchemaVersion()) + `"`)
	legacy := bytes.Replace(current, version, []byte(`"key":"export_version","value":"50"`), 1)
	require.NotEqual(t, current, legacy, "fixture must advertise the old export version")

	t.Run("sqlite", func(t *testing.T) {
		restored := openImportTargetDB(t)
		require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(legacy), restored))
		requireFederationReplayJSONL(t, restored, events)
	})
	t.Run("postgres", func(t *testing.T) {
		restored := openPostgresImportTarget(t)
		require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(legacy), restored))
		requireFederationReplayJSONL(t, restored, events)
	})
	t.Run("current-does-not-infer", func(t *testing.T) {
		restored := openImportTargetDB(t)
		require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(current), restored))
		due, err := restored.FederationLocalReplayDue(t.Context(), events[0].UID, true)
		require.NoError(t, err)
		require.False(t, due, "current backups restore only the recorded markers")
	})
}

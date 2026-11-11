package jsonl_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestFederationReplayJSONLRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			open := func() db.Storage {
				if backend == "postgres" {
					return openPostgresImportTarget(t)
				}
				return openImportTargetDB(t)
			}
			source, events := federationReplayJSONLFixture(t, true)
			backup := exportToBuffer(ctx, t, source)
			restored := open()
			require.NoError(t, jsonl.Import(ctx, backup, restored))
			requireFederationReplayJSONL(t, restored, events)

			if backend == "postgres" {
				var back bytes.Buffer
				require.NoError(t, jsonl.Export(ctx, restored, &back, jsonl.ExportOptions{IncludeDeleted: true}))
				roundTrip := openImportTargetDB(t)
				require.NoError(t, jsonl.Import(ctx, &back, roundTrip))
				requireFederationReplayJSONL(t, roundTrip, events)
			}

			var scoped bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, restored, &scoped, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: events[0].ProjectID}))
			scopedRestore := open()
			require.NoError(t, jsonl.Import(ctx, bytes.NewReader(scoped.Bytes()), scopedRestore))
			requireFederationReplayJSONL(t, scopedRestore, events)
			for _, opts := range []jsonl.ImportOptions{{NewInstance: true}, {MergeProject: true}} {
				target := open()
				require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(scoped.Bytes()), target, opts))
				_, recorded, err := target.FederationLocalReplayStatus(ctx, events[0].UID)
				require.NoError(t, err)
				require.False(t, recorded, "new instances and project merges do not inherit replay markers")
			}
		})
	}
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
	due, recorded, err := store.FederationLocalReplayStatus(ctx, events[0].UID)
	require.NoError(t, err)
	require.True(t, recorded)
	require.True(t, due, "a full restore preserves pending replay delivery")
	due, recorded, err = store.FederationLocalReplayStatus(ctx, events[1].UID)
	require.NoError(t, err)
	require.True(t, recorded)
	require.False(t, due, "a recorded quiet replay stays silent")
	_, recorded, err = store.FederationLocalReplayStatus(ctx, events[2].UID)
	require.NoError(t, err)
	require.False(t, recorded, "ordinary history must not become a replay")
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
	// Use the version-aware exporter used by read-only backups and cutovers.
	_, err = source.ExecContext(ctx, `UPDATE meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	legacy := exportToBuffer(ctx, t, source).Bytes()

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
	t.Run("scoped-does-not-infer", func(t *testing.T) {
		ctx := t.Context()
		var scoped bytes.Buffer
		require.NoError(t, jsonl.Export(ctx, source, &scoped, jsonl.ExportOptions{
			ProjectID: project.ID, IncludeDeleted: true,
		}))
		restored := openImportTargetDB(t)
		require.NoError(t, jsonl.Import(ctx, &scoped, restored))
		retained, err := restored.EventsByUIDs(ctx, project.ID, []string{events[0].UID})
		require.NoError(t, err)
		require.Len(t, retained, 1, "the scoped restore retains the unprojected event")
		_, recorded, err := restored.FederationLocalReplayStatus(ctx, events[0].UID)
		require.NoError(t, err)
		require.False(t, recorded, "scoped backups do not recover local replay markers")
	})
	t.Run("current-does-not-infer", func(t *testing.T) {
		restored := openImportTargetDB(t)
		require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(current), restored))
		_, recorded, err := restored.FederationLocalReplayStatus(t.Context(), events[0].UID)
		require.NoError(t, err)
		require.False(t, recorded, "current backups restore only the recorded markers")
	})
}

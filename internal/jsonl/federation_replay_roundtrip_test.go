package jsonl_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestFederationReplayJSONLRoundTrip(t *testing.T) {
	ctx := t.Context()
	source, events := federationReplayJSONLFixture(t)
	backup := exportToBuffer(ctx, t, source).Bytes()

	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup), restored))
	requireFederationReplayJSONL(t, restored, events)

	var scoped bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &scoped, jsonl.ExportOptions{ProjectID: events[0].ProjectID}))
	require.NotContains(t, scoped.String(), "federation_replay_due", "project exports omit replay delivery state")

	newInstance := openImportTargetDB(t)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(backup), newInstance, jsonl.ImportOptions{NewInstance: true}))
	due, err := newInstance.FederationLocalReplayDue(ctx, events[0].UID, true)
	require.NoError(t, err)
	require.False(t, due, "a new instance does not inherit local replay markers")
}

func TestFederationReplayJSONLRoundTripAcrossPostgres(t *testing.T) {
	postgres := openPostgresImportTarget(t)
	ctx := t.Context()
	source, events := federationReplayJSONLFixture(t)
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

func federationReplayJSONLFixture(t *testing.T) (*sqlitestore.Store, []db.Event) {
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
	_, err = source.ExecContext(ctx, `INSERT INTO federation_local_replays(event_uid, delivery_due) VALUES (?, 1), (?, 0)`, events[0].UID, events[1].UID)
	require.NoError(t, err)
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
	for _, event := range events {
		got, err := store.EventsByUIDs(ctx, event.ProjectID, []string{event.UID})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, event.ContentHash, got[0].ContentHash)
	}
}

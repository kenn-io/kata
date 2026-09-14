package dbtest

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkEmptyFederationAttachment(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, originalEvent, err := store.CreateProjectAndEvent(ctx, "empty-project", "user-a")
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)
	params := db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://hub.example/tasks", HubProjectID: 42,
		HubProjectUID: hubUID, ReplayHorizonEventID: 9, Actor: "user-a", EmptyOnly: true,
	}
	result, err := store.AdoptProjectIntoFederation(ctx, params)
	require.NoError(t, err)
	assert.Equal(t, project.ID, result.Project.ID)
	assert.Equal(t, params.HubProjectUID, result.Project.UID)
	assert.False(t, result.Binding.PushEnabled, "attachment must not import local history")
	assert.Zero(t, result.AdoptionSnapshotCount)
	require.NotNil(t, result.CreatedEvent, "local readers must learn the changed project identity")
	assert.Equal(t, result.CreatedEvent.ID, result.Binding.PushCursorEventID)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	for _, event := range events {
		assert.NotEqual(t, originalEvent.UID, event.UID, "an event hashed with the old UID cannot be replayed under the hub UID")
	}
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), result.Binding.PushCursorEventID, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	replayed, err := store.AdoptProjectIntoFederation(ctx, params)
	require.NoError(t, err)
	assert.Equal(t, result.Binding, replayed.Binding)
	assert.Nil(t, replayed.CreatedEvent, "exact replay must not emit another change")

	for _, kind := range []string{"issue", "metadata", "recurrence"} {
		t.Run(kind, func(t *testing.T) {
			local, err := store.CreateProject(ctx, "nonempty-"+kind)
			require.NoError(t, err)
			switch kind {
			case "issue":
				_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: local.ID, Title: "Keep this task", Author: "user-a"})
			case "metadata":
				_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: local.ID, Actor: "user-a", Patch: map[string]json.RawMessage{"purpose": json.RawMessage(`"keep this"`)}})
			case "recurrence":
				_, _, err = store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: local.ID, Actor: "user-a", Rule: "FREQ=DAILY", DTStart: "2030-01-01", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Keep this schedule"}})
			}
			require.NoError(t, err)
			attempt := params
			attempt.ProjectID = local.ID
			attempt.HubProjectUID, err = uid.New()
			require.NoError(t, err)
			_, err = store.AdoptProjectIntoFederation(ctx, attempt)
			require.ErrorIs(t, err, db.ErrFederationProjectNotEmpty)
			unchanged, err := store.ProjectByID(ctx, local.ID)
			require.NoError(t, err)
			assert.Equal(t, local.UID, unchanged.UID)
			_, err = store.FederationBindingByProject(ctx, local.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
	// Restored empty metadata need not use the writer's compact JSON spelling.
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{&db.ProjectExport{
		ID: 5, UID: replayProjectUID, Name: "restored-empty", CreatedAt: "2026-07-15T12:00:00.000Z",
		Metadata: json.RawMessage(`{ }`), Revision: 1,
	}}, db.ImportOptions{}))
	params.ProjectID = 5
	_, err = store.AdoptProjectIntoFederation(ctx, params)
	require.NoError(t, err)
	return nil
}

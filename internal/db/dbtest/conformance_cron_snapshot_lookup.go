package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCronJobSnapshotPayloadLookup(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	definition := cron.JobDefinition{
		Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"},
		Action: cron.Action{Kind: "notify", Recipient: "worker", Message: "Review"},
		Issue:  &cron.IssuePolicy{Kind: "existing", UID: issue.UID}, Overlap: "forbid", Catchup: "skip",
	}
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: definition})
	require.NoError(t, err)

	events := make([]db.RemoteEvent, 0, 257)
	for i := range 256 {
		versionUID, err := uid.New()
		require.NoError(t, err)
		events = append(events, cronJobSnapshotEvent(t, store, project, job, versionUID, definition, int64(310+i)))
	}
	wantVersionUID, err := uid.New()
	require.NoError(t, err)
	events = append(events, cronJobSnapshotEvent(t, store, project, job, wantVersionUID, definition, 1000))
	inserted, err := store.InsertRemoteEvents(ctx, project.ID, events)
	require.NoError(t, err)
	require.Len(t, inserted, len(events))
	for _, added := range inserted {
		require.True(t, added)
	}

	payload, err := store.CronJobSnapshotPayload(ctx, project.ID, job.UID, wantVersionUID)
	require.NoError(t, err)
	var got db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal([]byte(payload), &got))
	require.Equal(t, job.UID, got.UID)
	require.Equal(t, project.UID, got.ProjectUID)
	require.Equal(t, wantVersionUID, got.DefinitionEventUID)

	_, err = store.CronJobSnapshotPayload(ctx, project.ID, job.UID, job.DefinitionEventUID)
	require.ErrorIs(t, err, db.ErrNotFound, "unmatched history is not returned")
	other, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.CronJobSnapshotPayload(ctx, other.ID, job.UID, wantVersionUID)
	require.ErrorIs(t, err, db.ErrNotFound, "snapshot lookup is project scoped")
	return nil
}

func cronJobSnapshotEvent(t *testing.T, store db.Storage, project db.Project, job db.CronJob, versionUID string, definition cron.JobDefinition, physicalMS int64) db.RemoteEvent {
	t.Helper()
	rawDefinition, err := json.Marshal(definition)
	require.NoError(t, err)
	payload, err := json.Marshal(db.CronDefinitionEvent{
		UID: job.UID, ProjectUID: project.UID, Name: job.Name, Definition: jsontext.Value(rawDefinition),
		Author: job.Author, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
		DefinitionEventUID: versionUID, DefinitionHLC: &job.DefinitionHLC,
	})
	require.NoError(t, err)
	return newRemoteEvent(t, project, nil, "cron.job.snapshot", "worker", store.InstanceUID(), physicalMS, jsontext.Value(payload))
}

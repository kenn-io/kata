package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMoveCronMetadata(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, _, job, progress := recurringFixture(t, store, "latest")
	target, err := store.CreateProject(ctx, "cron-move-target")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = store.EnableProjectFederation(ctx, project.ID, "operator")
		require.NoError(t, err)
	}
	origin, err := uid.New()
	require.NoError(t, err)
	grants := map[string]int64{}
	for _, project := range []db.Project{source, target} {
		grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push,pull", Actor: "operator"})
		require.NoError(t, err)
		grants[project.UID] = grant.Enrollment.ID
	}
	for index, kind := range []string{"add", "alter", "remove", "unchanged"} {
		t.Run(kind, func(t *testing.T) {
			delivered, err := store.DispatchRecurringCron(ctx, db.DispatchRecurringCronIn{ProjectID: source.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(time.Duration(index+1) * 2 * time.Hour)})
			require.NoError(t, err)
			require.Len(t, delivered.Events, 2)
			issue, err := store.IssueByUID(ctx, *delivered.Events[0].IssueUID, db.IncludeDeletedNo)
			require.NoError(t, err)
			var metadata map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &metadata))
			require.Contains(t, metadata, "cron.occurrence", "provenance must originate in a real hub dispatch")
			switch kind {
			case "add":
				occurrence := metadata["cron.occurrence"]
				issue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Ordinary issue without delivery provenance", Author: "operator"})
				require.NoError(t, err)
				metadata = map[string]jsontext.Value{"cron.occurrence": occurrence}
			case "alter":
				var occurrence map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(metadata["cron.occurrence"], &occurrence))
				occurrence["scheduled_at"], err = json.Marshal(progress.Anchor.Add(24 * time.Hour))
				require.NoError(t, err)
				metadata["cron.occurrence"], err = json.Marshal(occurrence)
				require.NoError(t, err)
			case "remove":
				delete(metadata, "cron.occurrence")
			}
			snapshot := map[string]any{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "author": issue.Author, "status": issue.Status, "metadata": metadata}
			payload, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID, "to_project_uid": target.UID, "to_short_id": issue.ShortID, "snapshot": snapshot})
			require.NoError(t, err)
			event := newRemoteEvent(t, target, &issue.UID, "issue.moved", "operator", origin, time.Now().Add(time.Minute).UnixMilli()+int64(index), payload)
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			result, ingestErr := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: target.ID, SpokeInstanceUID: origin, BoundActor: "operator", FederationEnrollmentID: grants[target.UID], EventFeatures: db.SupportedEventFeatures, MoveSourceEnrollmentIDs: map[string]int64{source.UID: grants[source.UID]}, Events: []db.FederationIngestEvent{{SourceEventID: int64(index + 1), Event: event}}})
			got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			if kind == "unchanged" {
				require.NoError(t, ingestErr, "a move may preserve existing hub-owned provenance")
				require.Equal(t, 1, result.Accepted)
				require.Equal(t, target.ID, got.ProjectID)
				require.JSONEq(t, string(issue.Metadata), string(got.Metadata))
				return
			}
			assert.ErrorIs(t, ingestErr, db.ErrFederationIngestValidation, "a spoke move cannot rewrite hub-owned provenance")
			after, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			assert.Equal(t, before, after, "rejected provenance changes must append no events")
			assert.Equal(t, source.ID, got.ProjectID, "rejected provenance changes cannot move the issue")
			assert.JSONEq(t, string(issue.Metadata), string(got.Metadata), "trusted delivery provenance must remain unchanged")
		})
	}
	return nil
}

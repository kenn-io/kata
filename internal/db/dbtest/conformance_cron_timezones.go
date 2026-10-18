package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Model a definition accepted by an older release without applying write rules.
func storeLegacyLocalTimezone(t *testing.T, s db.Storage, job db.CronJob, enabled bool) db.CronJob {
	t.Helper()
	definition := job.Definition
	definition.Enabled = enabled
	definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "Local"}
	raw, err := json.Marshal(definition)
	require.NoError(t, err)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(t.Context(), `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), job.UID)
	require.NoError(t, err)
	job, err = s.CronJob(t.Context(), job.ProjectID, job.UID)
	require.NoError(t, err)
	return job
}

// Rewrite the original create event as well as the stored definition so a
// checkpoint models history genuinely accepted under the older rules.
func storeLegacyLocalTimezoneEvent(t *testing.T, s db.Storage, job db.CronJob) {
	t.Helper()
	events, err := s.EventsByUIDs(t.Context(), job.ProjectID, []string{job.DefinitionEventUID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	event := events[0]
	require.Equal(t, "cron.job.created", event.Type)
	var payload db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	payload.Definition, err = json.Marshal(job.Definition)
	require.NoError(t, err)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	hash, err := db.EventContentHash(db.EventHashInput{
		UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID,
		ProjectName: event.ProjectName, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat),
		Payload: jsontext.Value(raw),
	})
	require.NoError(t, err)
	query := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = query.ExecContext(t.Context(), `UPDATE events SET payload=$1, content_hash=$2 WHERE uid=$3`, string(raw), hash, event.UID)
	require.NoError(t, err)
}

func checkRecurringCronStoredNonPortableTimezoneKeepsDelivering(t *testing.T, s db.Storage) error {
	project, parent, job, progress := recurringFixture(t, s, "all")
	job = storeLegacyLocalTimezone(t, s, job, true)
	due := progress.Anchor.UTC().Truncate(24 * time.Hour).Add(9 * time.Hour)
	if !due.After(progress.Anchor) {
		due = due.Add(24 * time.Hour)
	}
	out, err := s.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: project.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: due})
	require.NoError(t, err)
	require.Len(t, out.Events, 2)
	children, err := s.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, children, 2)
	for _, child := range children {
		if child.ID != parent.ID {
			require.Equal(t, "Review service — "+due.Format(time.RFC3339Nano), child.Title)
		}
	}
	deleted, _, err := s.PutCronJob(t.Context(), db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator", Deleted: true})
	require.NoError(t, err)
	require.NotNil(t, deleted.DeletedAt)
	return nil
}

func checkRecurringCronNonPortableTimezoneWritesRejected(t *testing.T, s db.Storage) error {
	project, _, job, _ := recurringFixture(t, s, "all")
	for _, tc := range []struct {
		name                        string
		storedEnabled, writeEnabled bool
	}{
		{"unchanged", true, true}, {"disable", true, false}, {"enable", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := storeLegacyLocalTimezone(t, s, job, tc.storedEnabled)
			definition := stored.Definition
			definition.Enabled = tc.writeEnabled
			_, _, err := s.PutCronJob(t.Context(), db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: definition, ExpectedEventUID: stored.DefinitionEventUID, Actor: "operator"})
			require.ErrorIs(t, err, cron.ErrInvalid)
		})
	}
	t.Run("restore", func(t *testing.T) {
		stored := storeLegacyLocalTimezone(t, s, job, true)
		deleted, _, err := s.PutCronJob(t.Context(), db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: stored.Definition, ExpectedEventUID: stored.DefinitionEventUID, Actor: "operator", Deleted: true})
		require.NoError(t, err)
		_, _, err = s.PutCronJob(t.Context(), db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: stored.Definition, ExpectedEventUID: deleted.DefinitionEventUID, Actor: "operator"})
		require.ErrorIs(t, err, cron.ErrInvalid)
	})
	stored, err := s.CronJob(t.Context(), project.ID, job.UID)
	require.NoError(t, err)
	stored.Definition.Trigger.Timezone = "UTC"
	_, _, err = s.PutCronJob(t.Context(), db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: stored.Definition, ExpectedEventUID: stored.DefinitionEventUID, Actor: "operator"})
	require.NoError(t, err, "correcting the zone permits a live write")
	return nil
}

package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Run creation instants are stored at a fixed nanosecond width, so text order
// is time order. Equal instants tie-break by UID on both the first page and its
// cursor, and the database refuses a variable-width instant.
func checkRunHistoryInstants(t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	times := []string{"2026-10-06T09:00:00Z", "2026-10-06T09:00:00.001Z", "2026-10-06T09:00:00.001000001Z", "2026-10-06T09:00:00.01Z", "2026-10-06T09:00:00.1Z", "2026-10-06T09:00:00.100000000Z", "2026-10-06T09:00:00.100000001Z"}
	ids := []string{"01ARZ3NDEKTSV4RRFFQ69G5FA0", "01ARZ3NDEKTSV4RRFFQ69G5FA1", "01ARZ3NDEKTSV4RRFFQ69G5FA2", "01ARZ3NDEKTSV4RRFFQ69G5FA3", "01ARZ3NDEKTSV4RRFFQ69G5FA4", "01ARZ3NDEKTSV4RRFFQ69G5FA5", "01ARZ3NDEKTSV4RRFFQ69G5FA6"}
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for i, timestamp := range times {
		_, err := store.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: ids[i], JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "unknown", Summary: cron.Summary{Version: 1}})
		require.NoError(t, err)
		instant, err := time.Parse(time.RFC3339Nano, timestamp)
		require.NoError(t, err)
		fixed := instant.UTC().Format(db.CronRunCreatedAtFormat)
		if fixed != timestamp {
			_, err = q.ExecContext(t.Context(), "UPDATE cron_runs SET created_at=$1 WHERE uid=$2", timestamp, ids[i])
			require.Error(t, err, "variable-width instant %s must be refused", timestamp)
		}
		_, err = q.ExecContext(t.Context(), "UPDATE cron_runs SET created_at=$1 WHERE uid=$2", fixed, ids[i])
		require.NoError(t, err)
	}
	want := []string{ids[6], ids[5], ids[4], ids[3], ids[2], ids[1], ids[0]}
	for _, size := range []int{1, 2, 3, 100} {
		var got []string
		cursor := ""
		for pages := 0; pages < len(ids)+1; pages++ {
			page, err := store.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID, Limit: size, BeforeUID: cursor})
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			for _, run := range page {
				got = append(got, run.UID)
				for i, id := range ids {
					if id == run.UID {
						instant, err := time.Parse(time.RFC3339Nano, times[i])
						require.NoError(t, err)
						require.True(t, run.CreatedAt.Equal(instant), "nanosecond evidence survives the read")
					}
				}
			}
			cursor = page[len(page)-1].UID
		}
		require.Equal(t, want, got, "page size %d", size)
	}
	return nil
}

func checkIndependentRunObservations(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: definition, Actor: "worker"})
	require.NoError(t, err)
	occurrence := "daily:2026-10-06"
	inputs := []db.ObserveCronRun{}
	for range 2 {
		id, err := uid.New()
		require.NoError(t, err)
		in := db.ObserveCronRun{UID: id, ProjectID: project.ID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, IssueUID: &issue.UID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}}
		result, err := store.ObserveCronRun(ctx, in)
		require.NoError(t, err)
		require.Len(t, result.Events, 1)
		require.Equal(t, int64(1), result.Run.Revision)
		replay, err := store.ObserveCronRun(ctx, in)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		require.Empty(t, replay.Events)
		require.Equal(t, result.Run, replay.Run)
		inputs = append(inputs, in)
	}
	// Run identities accept lowercase input the way definition identities do.
	lowered := inputs[1]
	lowered.UID = strings.ToLower(lowered.UID)
	loweredReplay, err := store.ObserveCronRun(ctx, lowered)
	require.NoError(t, err)
	require.True(t, loweredReplay.Replayed)
	require.Equal(t, inputs[1].UID, loweredReplay.Run.UID)
	firstPage, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, JobUID: strings.ToLower(job.UID), Limit: 1})
	require.NoError(t, err)
	require.Len(t, firstPage, 1)
	secondPage, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, JobUID: strings.ToLower(job.UID), BeforeUID: strings.ToLower(firstPage[0].UID), Limit: 1})
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
	require.NotEqual(t, firstPage[0].UID, secondPage[0].UID)
	in := inputs[0]
	in.Status = "succeeded"
	in.ExpectedRevision = 1
	updated, err := store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Run.Revision)
	in.ExpectedRevision = 0
	replay, err := store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	in.Summary.Message = "Changed"
	_, err = store.ObserveCronRun(ctx, in)
	require.ErrorIs(t, err, db.ErrCronConflict)
	in = inputs[0]
	in.Actor = "other"
	in.ExpectedRevision = 2
	_, err = store.ObserveCronRun(ctx, in)
	require.ErrorIs(t, err, db.ErrCronConflict)
	_, err = store.CronRun(ctx, other.ID, inputs[0].UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	in = inputs[0]
	in.ProjectID = other.ID
	_, err = store.ObserveCronRun(ctx, in)
	require.ErrorIs(t, err, db.ErrCronConflict)
	first, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, first, 1)
	second, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, Limit: 1, BeforeUID: first[0].UID})
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.NotEqual(t, first[0].UID, second[0].UID)
	_, err = store.ListCronRuns(ctx, db.CronRunList{ProjectID: other.ID, BeforeUID: first[0].UID})
	require.Error(t, err)
	runs := []db.CronRunExport{}
	for run, err := range store.ExportCronRuns(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		runs = append(runs, run)
	}
	require.Len(t, runs, 2)
	jobs := []db.CronJobExport{}
	for job, err := range store.ExportCronJobs(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		jobs = append(jobs, job)
	}
	require.Len(t, jobs, 1)
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Review workflow", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "review", Kind: "command", Command: "git status"}}}})
	require.NoError(t, err)
	workflows := []db.CronWorkflowExport{}
	for value, err := range store.ExportCronWorkflows(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		workflows = append(workflows, value)
	}
	require.Len(t, workflows, 1)
	require.Equal(t, workflow.UID, workflows[0].UID)
	workflowRunUID, err := uid.New()
	require.NoError(t, err)
	workflowRun, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: workflowRunUID, WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker", Status: "unknown", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	require.Nil(t, workflowRun.Run.JobUID)
	require.Equal(t, &workflow.UID, workflowRun.Run.WorkflowUID)

	return nil
}

func checkCronRunRevisionAfterRemoteWinner(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, project)
	runUID, err := uid.New()
	require.NoError(t, err)
	input := db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}}
	created, err := store.ObserveCronRun(ctx, input)
	require.NoError(t, err)
	input.Status = "succeeded"
	input.Summary.Message = "local evidence"
	input.ExpectedRevision = created.Run.Revision
	localUpdate, err := store.ObserveCronRun(ctx, input)
	require.NoError(t, err)
	require.Equal(t, int64(2), localUpdate.Run.Revision)

	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	originUID, err := uid.New()
	require.NoError(t, err)
	eventUID, err := uid.New()
	require.NoError(t, err)
	remoteRun := localUpdate.Run
	remoteRun.Status = "failed"
	remoteRun.Summary.Message = "peer evidence"
	remoteRun.Revision = 2
	remoteRun.UpdatedAt = localUpdate.Run.UpdatedAt.Add(time.Minute)
	payload, err := json.Marshal(db.NewCronRunObservation(remoteRun, project.UID))
	require.NoError(t, err)
	remoteEvent := db.RemoteEvent{
		EventUID: eventUID, OriginInstanceUID: originUID, ProjectUID: project.UID,
		ProjectName: project.Name, Type: "cron.run.observed", Actor: remoteRun.Actor,
		CreatedAt: remoteRun.UpdatedAt, HLCPhysicalMS: remoteRun.UpdatedAt.UnixMilli(),
		Payload: jsontext.Value(payload),
	}
	resignNativeEvent(t, &remoteEvent)
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, remoteEvent)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))

	actual, err := store.CronRun(ctx, project.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, "failed", actual.Status)
	require.Equal(t, int64(3), actual.Revision, "a winning remote evidence replacement invalidates clients holding the previous revision")

	// Replaying the same event history must retain the local CAS token.
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	actual, err = store.CronRun(ctx, project.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, "failed", actual.Status)
	require.Equal(t, int64(3), actual.Revision)

	stale := input
	stale.Status = "succeeded"
	stale.Summary.Message = "late stale update"
	stale.ExpectedRevision = localUpdate.Run.Revision
	_, err = store.ObserveCronRun(ctx, stale)
	require.ErrorIs(t, err, db.ErrCronConflict, "a client holding revision 2 cannot replace unseen peer evidence")
	return nil
}

func checkIssuePlanningDates(t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker", Metadata: map[string]jsontext.Value{"scheduled_on": []byte(`"2026-10-06"`), "deadline_on": []byte(`"2026-10-07T13:00:00Z"`)}})
	require.NoError(t, err)
	before, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	dates, err := store.IssuePlanningDates(t.Context(), db.IssuePlanningDatesIn{ProjectID: project.ID, IssueID: issue.ID, DefaultTimezone: "Europe/Paris"})
	require.NoError(t, err)
	require.Equal(t, issue.UID, dates.IssueUID)
	require.Equal(t, issue.Revision, dates.Revision)
	require.Equal(t, "Europe/Paris", dates.ScheduledOn.Timezone)
	require.Equal(t, "2026-10-05T22:00:00Z", dates.ScheduledOn.Instant.Format("2006-01-02T15:04:05Z"))
	require.Equal(t, "UTC", dates.DeadlineOn.Timezone)
	after, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = store.IssuePlanningDates(t.Context(), db.IssuePlanningDatesIn{ProjectID: project.ID + 100, IssueID: issue.ID})
	require.ErrorIs(t, err, db.ErrNotFound)

	scheduled, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Recurring review", Author: "worker"})
	require.NoError(t, err)
	_, err = store.CreateRecurrenceForIssue(t.Context(), db.CreateRecurrenceForIssueIn{IssueID: scheduled.ID, Recurrence: db.CreateRecurrenceIn{ProjectID: project.ID, Actor: "worker", Rule: "FREQ=DAILY", DTStart: "2026-03-07", Timezone: "America/New_York", Template: db.RecurrenceTemplate{Title: "Recurring review"}}})
	require.NoError(t, err)
	for _, tc := range []struct{ value, instant string }{{"2026-03-08T02:30", "2026-03-08T07:00:00Z"}, {"2026-11-01T01:30", "2026-11-01T05:30:00Z"}} {
		raw, err := json.Marshal(tc.value)
		require.NoError(t, err)
		patched, err := store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: scheduled.ID, Actor: "worker", Patch: map[string]jsontext.Value{"timezone": []byte(`null`), "scheduled_on": raw, "deadline_on": []byte(`"2026-10-07"`)}})
		require.NoError(t, err)
		cursor, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		dates, err := store.IssuePlanningDates(t.Context(), db.IssuePlanningDatesIn{ProjectID: project.ID, IssueID: scheduled.ID, DefaultTimezone: "Europe/Paris"})
		require.NoError(t, err)
		require.Equal(t, patched.Issue.Revision, dates.Revision)
		require.Equal(t, "America/New_York", dates.ScheduledOn.Timezone)
		require.Equal(t, tc.instant, dates.ScheduledOn.Instant.Format("2006-01-02T15:04:05Z"))
		require.Equal(t, "Europe/Paris", dates.DeadlineOn.Timezone, "deadline never inherits recurrence timezone")
		after, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, cursor, after)
	}
	return nil
}

package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/uid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func nativeFederationDefinitions(t *testing.T, store db.Storage, project db.Project) (db.CronJob, db.CronWorkflow) {
	return nativeFederationDefinitionsAs(t, store, project, "worker")
}

func nativeFederationDefinitionsAs(t *testing.T, store db.Storage, project db.Project, actor string) (db.CronJob, db.CronWorkflow) {
	t.Helper()
	workflow, _, err := store.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Review workflow", Actor: actor, Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}})
	require.NoError(t, err)
	job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review job", Actor: actor, Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", WorkflowUID: workflow.UID}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}})
	require.NoError(t, err)
	return job, workflow
}

func portableNativeEvent(event db.Event) db.RemoteEvent {
	return db.RemoteEvent{EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt, Payload: jsontext.Value(event.Payload), ContentHash: event.ContentHash, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID}
}

func checkNativeCronFederation(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, hub, project)
	binding, err := hub.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	baseline, err := hub.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, AfterID: binding.ReplayHorizonEventID - 1, Limit: 100})
	require.NoError(t, err)
	require.Contains(t, eventTypeNames(baseline), "cron.job.snapshot")
	require.Contains(t, eventTypeNames(baseline), "cron.workflow.snapshot")
	lastBaseline, err := hub.MaxFederationBaselineEventID(ctx, project.ID, binding.ReplayHorizonEventID)
	require.NoError(t, err)
	require.Equal(t, baseline[len(baseline)-1].ID, lastBaseline, "cron snapshots belong to the bootstrap boundary")
	metadata, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.CronEventFeature})
	require.NoError(t, err)
	require.Equal(t, lastBaseline, metadata.BaselineThroughEventID)
	require.Equal(t, db.CronEventFeature, metadata.RequiredFeatures)
	page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: binding.ReplayHorizonEventID - 1, Features: db.CronEventFeature, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, baseline, page.Events)
	// Fresh peers inherit pre-federation definitions. Each page is materialized
	// separately, including the deliberately missing workflow dependency first.
	for _, peerName := range []string{"first-peer", "second-peer"} {
		t.Run(peerName, func(t *testing.T) {
			peer := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, peer.Close()) })
			local, err := peer.CreateProject(ctx, "spoke-project")
			require.NoError(t, err)
			adopted, err := peer.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "worker", EmptyOnly: true})
			require.NoError(t, err)
			var jobEvent, workflowEvent db.Event
			for _, event := range baseline {
				if event.Type == "cron.job.snapshot" {
					jobEvent = event
				}
				if event.Type == "cron.workflow.snapshot" {
					workflowEvent = event
				}
			}
			for _, event := range []db.Event{jobEvent, workflowEvent} {
				inserted, err := peer.InsertRemoteEvent(ctx, local.ID, portableNativeEvent(event))
				require.NoError(t, err)
				require.True(t, inserted)
				if err := peer.MaterializeFederatedProject(ctx, local.ID); err != nil {
					require.NoError(t, err)
				}
				visible, err := peer.CronJob(ctx, local.ID, job.UID)
				require.NoError(t, err, "unresolved jobs stay visible")
				query := peer.(interface {
					QueryRowContext(context.Context, string, ...any) *sql.Row
				})
				dependencyErr := db.CheckCronDependencies(ctx, query, local.ID, visible.Definition)
				if event.Type == "cron.job.snapshot" {
					require.ErrorContains(t, dependencyErr, "missing workflow dependency")
				} else {
					require.NoError(t, dependencyErr)
				}
				inserted, err = peer.InsertRemoteEvent(ctx, local.ID, portableNativeEvent(event))
				require.NoError(t, err)
				require.False(t, inserted)
			}
			actual, err := peer.CronJob(ctx, local.ID, job.UID)
			require.NoError(t, err)
			require.Equal(t, job.Definition, actual.Definition)
			require.Equal(t, job.DefinitionEventUID, actual.DefinitionEventUID)
			require.Equal(t, job.DefinitionHLC, actual.DefinitionHLC)
			actualWorkflow, err := peer.CronWorkflow(ctx, local.ID, workflow.UID)
			require.NoError(t, err)
			require.Equal(t, workflow.Definition, actualWorkflow.Definition)
			deleted := actualWorkflow
			deleted.DeletedAt = new(time.Now().UTC())
			payload, err := json.Marshal(db.CronDefinitionEvent{UID: deleted.UID, ProjectUID: project.UID, Name: deleted.Name, Definition: mustNativeJSON(t, deleted.Definition), Author: deleted.Author, CreatedAt: deleted.CreatedAt, UpdatedAt: deleted.UpdatedAt, DeletedAt: deleted.DeletedAt})
			require.NoError(t, err)
			deleteEvent := portableNativeEvent(workflowEvent)
			deleteEvent.Type = "cron.workflow.deleted"
			deleteEvent.EventUID, err = uid.New()
			require.NoError(t, err)
			deleteEvent.HLCPhysicalMS++
			deleteEvent.Payload = payload
			resignNativeEvent(t, &deleteEvent)
			_, err = peer.InsertRemoteEvent(ctx, local.ID, deleteEvent)
			require.NoError(t, err)
			if err := peer.MaterializeFederatedProject(ctx, local.ID); err != nil {
				require.NoError(t, err)
			}
			visible, err := peer.CronJob(ctx, local.ID, job.UID)
			require.NoError(t, err)
			require.ErrorContains(t, db.CheckCronDependencies(ctx, peer.(interface {
				QueryRowContext(context.Context, string, ...any) *sql.Row
			}), local.ID, visible.Definition), "tombstoned workflow dependency")
			require.NoError(t, peer.ResetFederatedProject(ctx, local.ID, adopted.Binding.ReplayHorizonEventID, adopted.Binding.PullCursorEventID))
			_, err = peer.CronJob(ctx, local.ID, job.UID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
	return nil
}

func mustNativeJSON(t *testing.T, value any) jsontext.Value {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func resignNativeEvent(t *testing.T, event *db.RemoteEvent) {
	t.Helper()
	var err error
	event.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: event.Payload, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID})
	require.NoError(t, err)
}

func checkNativeCronPush(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	local, err := spoke.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	adopted, err := spoke.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", EmptyOnly: true})
	require.NoError(t, err)
	_, err = spoke.EnableFederationPush(ctx, local.ID, adopted.Binding.PushCursorEventID)
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitionsAs(t, spoke, adopted.Project, "requester")
	require.Equal(t, "worker", job.Author)
	require.Equal(t, "worker", workflow.Author)
	events, err := spoke.PendingFederationPushEvents(ctx, local.ID, spoke.InstanceUID(), adopted.Binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, events, 2, "both cron definitions must be push eligible")
	batch := make([]db.FederationIngestEvent, 0, len(events))
	for _, event := range events {
		batch = append(batch, db.FederationIngestEvent{SourceEventID: event.ID, Event: portableNativeEvent(event)})
	}
	t.Run("created definition author is enrolled actor", func(t *testing.T) {
		invalid := batch[0]
		var payload db.CronDefinitionEvent
		require.NoError(t, json.Unmarshal(invalid.Event.Payload, &payload))
		payload.Author = "different-author"
		invalid.Event.Payload = mustNativeJSON(t, payload)
		resignNativeEvent(t, &invalid.Event)
		_, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: project.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", Events: []db.FederationIngestEvent{invalid}})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	})
	result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: project.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", Events: batch})
	require.NoError(t, err)
	require.Equal(t, 2, result.Accepted)
	actual, err := store.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, job.DefinitionEventUID, actual.DefinitionEventUID)
	actualWorkflow, err := store.CronWorkflow(ctx, project.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, workflow.DefinitionEventUID, actualWorkflow.DefinitionEventUID)
	duplicate, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: project.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", Events: batch})
	require.NoError(t, err)
	require.Equal(t, 2, duplicate.Duplicates)
	runUID, err := uid.New()
	require.NoError(t, err)
	occurrence := "daily:2026-10-06"
	observed, err := spoke.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: local.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: "requester", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	require.Equal(t, "worker", observed.Run.Actor)
	require.Len(t, observed.Events, 1)
	require.Equal(t, "worker", observed.Events[0].Actor)
	var runPayload struct {
		Actor string `json:"actor"`
	}
	require.NoError(t, json.Unmarshal([]byte(observed.Events[0].Payload), &runPayload))
	require.Equal(t, observed.Events[0].Actor, runPayload.Actor)
	runBatch := []db.FederationIngestEvent{{SourceEventID: observed.Events[0].ID, Event: portableNativeEvent(observed.Events[0])}}
	runIngest, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: project.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", Events: runBatch})
	require.NoError(t, err)
	require.Equal(t, 1, runIngest.Accepted)
	actualRun, err := store.CronRun(ctx, project.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, "worker", actualRun.Actor)
	return nil
}

func checkNativeCronAdoptionAuthor(t *testing.T, spoke db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := spoke.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, spoke, project)
	runs := []db.CronRun{}
	occurrence := "daily:2026-10-06"
	// Run actors are observation history, so adoption keeps them even after
	// the author grant is consumed.
	for _, observer := range []string{"operator", "worker-1"} {
		runUID, err := uid.New()
		require.NoError(t, err)
		result, err := spoke.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: observer, Status: "running", Summary: cron.Summary{Version: 1, InputTokens: 1<<63 - 1}})
		require.NoError(t, err)
		runs = append(runs, result.Run)
	}

	hub := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hub.Close()) })
	target, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, target.ID, "operator")
	require.NoError(t, err)
	enrollment, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &target.ID, Actor: "operator", Capabilities: "pull,push", AllowAdoptionSnapshotAuthors: true})
	require.NoError(t, err)
	adopted, err := spoke.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: target.ID, HubProjectUID: target.UID, Actor: "operator"})
	require.NoError(t, err)
	events, err := spoke.PendingFederationPushEvents(ctx, project.ID, spoke.InstanceUID(), adopted.Binding.PushCursorEventID, 100)
	require.NoError(t, err)
	batch := []db.FederationIngestEvent{}
	for _, event := range events {
		batch = append(batch, db.FederationIngestEvent{SourceEventID: event.ID, Event: portableNativeEvent(event)})
	}
	params := db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: target.ID, FederationEnrollmentID: enrollment.Enrollment.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "operator", AllowSnapshotAuthorPreservation: true, AdoptionBaseline: db.FederationAdoptionBaselineOpen, AdoptionBaselineEndSourceEventID: events[len(events)-1].ID, Events: batch[:1]}
	_, err = hub.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	// Existing continuation policy can consume author preservation before the
	// terminal chunk. Its remaining definitions receive canonical authors.
	_, err = hub.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}).ExecContext(ctx, `UPDATE federation_enrollments SET allow_adoption_snapshot_authors=0 WHERE id=$1`, enrollment.Enrollment.ID)
	require.NoError(t, err)
	params.AdoptionBaseline = db.FederationAdoptionBaselineComplete
	params.Events = batch[1:]
	_, err = hub.IngestFederationEvents(ctx, params)
	require.NoError(t, err)
	for _, run := range runs {
		actual, err := hub.CronRun(ctx, target.ID, run.UID)
		require.NoError(t, err)
		require.Equal(t, run.Summary, actual.Summary, "adoption preserves exact integer evidence")
		require.Equal(t, run.Actor, actual.Actor)
		require.Equal(t, run.CreatedAt, actual.CreatedAt)
	}
	actual, err := hub.CronWorkflow(ctx, target.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, "operator", actual.Author)
	job, jobEvents, err := spoke.PutCronJob(ctx, db.PutCronJob{
		ProjectID: project.ID, UID: job.UID, Name: "Edited review",
		Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "editor",
	})
	require.NoError(t, err)
	workflow, workflowEvent, err := spoke.PutCronWorkflow(ctx, db.PutCronWorkflow{
		ProjectID: project.ID, UID: workflow.UID, Name: "Edited workflow",
		Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: "editor",
	})
	require.NoError(t, err)
	require.Equal(t, "worker", job.Author, "the spoke has not received the canonical adoption snapshot yet")
	require.Equal(t, "worker", workflow.Author, "the spoke has not received the canonical adoption snapshot yet")
	editEvents := append(jobEvents, workflowEvent)
	for _, edit := range editEvents {
		_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{
			EventFeatures: db.CronEventFeature, ProjectID: target.ID,
			FederationEnrollmentID: enrollment.Enrollment.ID, SpokeInstanceUID: spoke.InstanceUID(),
			BoundActor: "operator", Events: []db.FederationIngestEvent{{
				SourceEventID: edit.ID, Event: portableNativeEvent(edit),
			}},
		})
		require.NoError(t, err)
		events = append(events, edit)
	}
	canonicalJob, err := hub.CronJob(ctx, target.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, "operator", canonicalJob.Author, "definition edits cannot replace adoption authorship")
	canonicalWorkflow, err := hub.CronWorkflow(ctx, target.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, "operator", canonicalWorkflow.Author, "workflow edits cannot replace adoption authorship")
	for _, source := range events {
		canonical, err := hub.EventsByUIDs(ctx, target.ID, []string{source.UID})
		require.NoError(t, err)
		_, err = spoke.ReconcileLocalFederationEcho(ctx, project.ID, portableNativeEvent(canonical[0]))
		require.NoError(t, err)
	}
	if err := spoke.MaterializeFederatedProject(ctx, project.ID); err != nil {
		require.NoError(t, err)
	}
	actual, err = spoke.CronWorkflow(ctx, project.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, "operator", actual.Author)
	actualJob, err := spoke.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, "operator", actualJob.Author)
	require.Equal(t, "Edited review", actualJob.Name)
	require.Equal(t, workflow.DefinitionEventUID, actual.DefinitionEventUID)
	for _, run := range runs {
		echoed, err := spoke.CronRun(ctx, project.ID, run.UID)
		require.NoError(t, err)
		require.Equal(t, run.Actor, echoed.Actor)
	}
	// Outside an adoption baseline, a run snapshot may only continue a run under
	// its stored actor or the bound actor; it cannot reattribute the run.
	for _, source := range events {
		if source.Type != "cron.run.snapshot" || !strings.Contains(source.Payload, `"actor":"worker-1"`) {
			continue
		}
		stray := portableNativeEvent(source)
		stray.Payload = jsontext.Value(strings.Replace(string(stray.Payload), `"actor":"worker-1"`, `"actor":"intruder"`, 1))
		stray.EventUID, err = uid.New()
		require.NoError(t, err)
		stray.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: stray.EventUID, OriginInstanceUID: stray.OriginInstanceUID, ProjectUID: stray.ProjectUID, ProjectName: stray.ProjectName, Type: stray.Type, Actor: stray.Actor, HLCPhysicalMS: stray.HLCPhysicalMS, HLCCounter: stray.HLCCounter, CreatedAt: stray.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: stray.Payload})
		require.NoError(t, err)
		_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: target.ID, FederationEnrollmentID: enrollment.Enrollment.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "operator", Events: []db.FederationIngestEvent{{SourceEventID: events[len(events)-1].ID + 1, Event: stray}}})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		require.ErrorContains(t, err, "run actor differs from bound actor")
		return nil
	}
	t.Fatal("adoption baseline has no run snapshot observed by worker-1")
	return nil
}

func checkNativeCronValidation(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	nativeFederationDefinitions(t, store, project)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	var original db.Event
	for _, event := range events {
		if event.Type == "cron.job.created" {
			original = event
		}
	}
	peer := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, peer.Close()) })
	local, err := peer.CreateProjectWithUID(ctx, "spoke-project", project.UID)
	require.NoError(t, err)
	for _, mutate := range []struct {
		name   string
		change func(*db.RemoteEvent)
	}{
		{"unsupported event", func(e *db.RemoteEvent) { e.Type = "cron.job.future" }},
		{"invalid definition", func(e *db.RemoteEvent) {
			e.Payload = jsontext.Value(`{"uid":"` + original.UID + `","project_uid":"` + project.UID + `","name":"Invalid","definition":{"version":999}}`)
		}},
		{"wrong project", func(e *db.RemoteEvent) { e.ProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA" }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			event := portableNativeEvent(original)
			event.EventUID, err = uid.New()
			require.NoError(t, err)
			mutate.change(&event)
			event.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: event.Payload})
			require.NoError(t, err)
			_, err := peer.InsertRemoteEvent(ctx, local.ID, event)
			require.Error(t, err)
			found, err := peer.EventsByUIDs(ctx, local.ID, []string{event.EventUID})
			require.ErrorIs(t, err, db.ErrNotFound)
			require.Empty(t, found)
		})
	}
	return nil
}

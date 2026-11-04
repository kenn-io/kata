package dbtest

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// Attribution follows the winning event, while author remains the creator.
// All definition surfaces must agree, including mutation readbacks and tombstones.
func checkCronDefinitionEditors(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitionsAs(t, store, project, "creator")
	check := func(actor string, deleted bool) {
		t.Helper()
		for _, value := range []db.CronDefinition{job.CronDefinition, workflow.CronDefinition} {
			require.Equal(t, "creator", value.Author)
			require.Equal(t, &actor, value.UpdatedBy)
			require.Equal(t, deleted, value.DeletedAt != nil)
		}
		readJob, err := store.CronJob(ctx, project.ID, strings.ToLower(job.UID))
		require.NoError(t, err)
		require.Equal(t, job, readJob)
		readWorkflow, err := store.CronWorkflow(ctx, project.ID, strings.ToLower(workflow.UID))
		require.NoError(t, err)
		require.Equal(t, workflow, readWorkflow)
		jobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID, IncludeDeleted: true})
		require.NoError(t, err)
		require.Equal(t, []db.CronJob{job}, jobs)
		workflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID, IncludeDeleted: true})
		require.NoError(t, err)
		require.Equal(t, []db.CronWorkflow{workflow}, workflows)
		liveJobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
		require.NoError(t, err)
		liveWorkflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID})
		require.NoError(t, err)
		if deleted {
			require.Empty(t, liveJobs)
			require.Empty(t, liveWorkflows)
		} else {
			require.Equal(t, jobs, liveJobs)
			require.Equal(t, workflows, liveWorkflows)
		}
	}
	check("creator", false)
	for _, action := range []struct {
		actor   string
		deleted bool
	}{{"editor", false}, {"archiver", true}, {"restorer", false}} {
		// Delete the referencing job before the workflow; restore in reverse order.
		putJob := func() {
			var events []db.Event
			job, events, err = store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: action.actor, Deleted: action.deleted})
			require.NoError(t, err)
			require.Equal(t, action.actor, events[0].Actor)
		}
		if action.deleted {
			putJob()
		}
		workflow, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: action.actor, Deleted: action.deleted})
		require.NoError(t, err)
		if !action.deleted {
			putJob()
		}
		check(action.actor, action.deleted)
	}
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	_, err = store.CronJob(ctx, other.ID, job.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.CronWorkflow(ctx, other.ID, workflow.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	otherJobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: other.ID})
	require.NoError(t, err)
	require.Empty(t, otherJobs)
	otherWorkflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: other.ID})
	require.NoError(t, err)
	require.Empty(t, otherWorkflows)
	for _, record := range []any{db.CronJobExport(job), db.CronWorkflowExport(workflow)} {
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		require.NotContains(t, fields, "updated_by", "derived attribution must not change backup records")
	}
	return nil
}

// Retained events can arrive after a snapshot. Only the exact winner fills
// missing attribution; a later-arriving losing edit cannot replace it.
func checkCronDefinitionEditorHistory(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitionsAs(t, source, project, "creator")
	creates, err := source.EventsByUIDs(ctx, project.ID, []string{job.DefinitionEventUID, workflow.DefinitionEventUID})
	require.NoError(t, err)
	job, jobEvents, err := source.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "winning-editor"})
	require.NoError(t, err)
	workflow, workflowEvent, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: "winning-editor"})
	require.NoError(t, err)
	binding, err := source.EnableProjectFederation(ctx, project.ID, "snapshotter")
	require.NoError(t, err)
	baseline, err := source.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, AfterID: binding.ReplayHorizonEventID - 1, Limit: 100})
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		peer := backend.Open(t)
		defer func() { require.NoError(ht, peer.Close()) }()
		local, err := peer.CreateProject(ctx, "spoke-project")
		require.NoError(ht, err)
		_, err = peer.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "bound-actor", EmptyOnly: true})
		require.NoError(ht, err)
		insert := func(events []db.Event) {
			for _, event := range events {
				_, err := peer.InsertRemoteEvent(ctx, local.ID, portableNativeEvent(event))
				require.NoError(ht, err)
			}
			require.NoError(ht, peer.MaterializeFederatedProject(ctx, local.ID))
		}
		check := func(editor *string) {
			gotJob, err := peer.CronJob(ctx, local.ID, job.UID)
			require.NoError(ht, err)
			gotWorkflow, err := peer.CronWorkflow(ctx, local.ID, workflow.UID)
			require.NoError(ht, err)
			require.Equal(ht, editor, gotJob.UpdatedBy)
			require.Equal(ht, editor, gotWorkflow.UpdatedBy)
			require.Equal(ht, job.DefinitionEventUID, gotJob.DefinitionEventUID)
			require.Equal(ht, workflow.DefinitionEventUID, gotWorkflow.DefinitionEventUID)
		}
		insert(baseline)
		check(nil)
		// Generate both retained-history arrival orders. Replays remain inert.
		winnerFirst := hegel.Draw(ht, hegel.Booleans())
		winners := []db.Event{jobEvents[0], workflowEvent}
		if winnerFirst {
			insert(winners)
			check(new("winning-editor"))
			insert(creates)
		} else {
			insert(creates)
			check(nil)
			insert(winners)
		}
		check(new("winning-editor"))
		insert(winners)
		check(new("winning-editor"))
	}, hegel.WithTestCases(4), hegel.WithDatabase(t.TempDir()))
	return nil
}

// Any representable actor, including Unicode and whitespace within the name,
// must survive the join exactly. PostgreSQL TEXT excludes NUL. Bound the
// materialized fixture size, not the generator's Unicode domain.
func checkCronDefinitionEditorProperty(t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		text := []rune(hegel.Draw(ht, hegel.Text().ExcludeCharacters("\x00")))
		if len(text) > 128 {
			text = text[:128]
		}
		actor := "editor-" + strings.TrimSpace(string(text))
		job, workflow := nativeFederationDefinitionsAs(t, store, project, "creator")
		edits := min(hegel.Draw(ht, hegel.Integers(1, math.MaxInt)), 4)
		for edit := range edits {
			latest := fmt.Sprintf("%s-%d", actor, edit)
			job, _, err = store.PutCronJob(t.Context(), db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: latest})
			require.NoError(ht, err)
			workflow, _, err = store.PutCronWorkflow(t.Context(), db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: latest})
			require.NoError(ht, err)
		}
		actor = fmt.Sprintf("%s-%d", actor, edits-1)
		readJob, err := store.CronJob(t.Context(), project.ID, job.UID)
		require.NoError(ht, err)
		readWorkflow, err := store.CronWorkflow(t.Context(), project.ID, workflow.UID)
		require.NoError(ht, err)
		jobs, err := store.ListCronJobs(t.Context(), db.CronList{ProjectID: project.ID})
		require.NoError(ht, err)
		workflows, err := store.ListCronWorkflows(t.Context(), db.CronList{ProjectID: project.ID})
		require.NoError(ht, err)
		require.Contains(ht, jobs, readJob)
		require.Contains(ht, workflows, readWorkflow)
		for _, value := range []db.CronDefinition{job.CronDefinition, workflow.CronDefinition, readJob.CronDefinition, readWorkflow.CronDefinition} {
			require.Equal(ht, &actor, value.UpdatedBy)
			require.Equal(ht, "creator", value.Author)
		}
	}, hegel.WithTestCases(12), hegel.WithDatabase(t.TempDir()))
	return nil
}

// A real adoption baseline retains the winning event UID but replaces the
// original event history. Its snapshot actor must never become the last editor.
func checkCronDefinitionEditorBaseline(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitionsAs(t, store, project, "creator")
	job, _, err = store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "editor"})
	require.NoError(t, err)
	workflow, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: "editor"})
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: hubUID, ReplayHorizonEventID: 1, Actor: "snapshot-actor"})
	require.NoError(t, err)
	readJob, err := store.CronJob(ctx, adopted.Project.ID, job.UID)
	require.NoError(t, err)
	readWorkflow, err := store.CronWorkflow(ctx, adopted.Project.ID, workflow.UID)
	require.NoError(t, err)
	for _, value := range []db.CronDefinition{readJob.CronDefinition, readWorkflow.CronDefinition} {
		require.Nil(t, value.UpdatedBy)
		require.Equal(t, "creator", value.Author)
	}
	require.Equal(t, job.DefinitionEventUID, readJob.DefinitionEventUID)
	require.Equal(t, workflow.DefinitionEventUID, readWorkflow.DefinitionEventUID)
	jobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Nil(t, jobs[0].UpdatedBy)
	workflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, workflows, 1)
	require.Nil(t, workflows[0].UpdatedBy)
	job, _, err = store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "next-editor"})
	require.NoError(t, err)
	workflow, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: "next-editor"})
	require.NoError(t, err)
	// Adoption binds subsequent writes to the enrollment actor, even when a
	// caller requests a different name. Report the event actor, not that input.
	require.Equal(t, new("snapshot-actor"), job.UpdatedBy)
	require.Equal(t, new("snapshot-actor"), workflow.UpdatedBy)
	return nil
}

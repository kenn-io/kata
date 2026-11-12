package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A fresh running observation requires the adapter to understand every known
// workflow it may execute. Existing and buffered evidence remains admissible.
func checkRunArgvAdmission(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, store, project)
	_, other := nativeFederationDefinitions(t, store, project)
	newID := func() string { id, err := uid.New(); require.NoError(t, err); return id }
	base := db.ObserveCronRun{ProjectID: project.ID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}}
	ordinary := base
	ordinary.UID, ordinary.JobUID, ordinary.DefinitionEventUID = newID(), &job.UID, &job.DefinitionEventUID
	_, err = store.ObserveCronRun(ctx, ordinary)
	require.NoError(t, err)

	prompt, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Prompt workflow", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "ask", Kind: "prompt", Prompt: "Review"}}}})
	require.NoError(t, err)
	promptRun := base
	promptRun.UID, promptRun.WorkflowUID, promptRun.WorkflowDefinitionEventUID = newID(), &prompt.UID, &prompt.DefinitionEventUID
	_, err = store.ObserveCronRun(ctx, promptRun)
	require.NoError(t, err)

	argv := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "exec", Kind: "argv", Argv: []string{"echo", "hello"}}}}
	workflow, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: workflow.UID, ExpectedEventUID: workflow.DefinitionEventUID, Name: workflow.Name, Actor: "worker", Definition: argv})
	require.NoError(t, err)
	// Keep an argv revision after a later command edit.
	historical, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Historical workflow", Actor: "worker", Definition: argv})
	require.NoError(t, err)
	oldEvent := historical.DefinitionEventUID
	_, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: historical.UID, ExpectedEventUID: oldEvent, Name: "Edited workflow", Actor: "worker", Definition: other.Definition})
	require.NoError(t, err)

	for _, path := range []string{"direct", "historical", "job", "job with unrelated workflow"} {
		t.Run(path, func(t *testing.T) {
			in := base
			in.UID = newID()
			switch path {
			case "direct":
				in.WorkflowUID, in.WorkflowDefinitionEventUID = &workflow.UID, &workflow.DefinitionEventUID
			case "historical":
				in.WorkflowUID, in.WorkflowDefinitionEventUID = &historical.UID, &oldEvent
			case "job":
				in.JobUID, in.DefinitionEventUID = &job.UID, &job.DefinitionEventUID
			default:
				in.JobUID, in.DefinitionEventUID = &job.UID, &job.DefinitionEventUID
				in.WorkflowUID, in.WorkflowDefinitionEventUID = &other.UID, &other.DefinitionEventUID
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			for _, features := range []string{"", db.CronWorkflowArgvEventFeature + "-extra"} {
				in.EventFeatures = features
				_, err = store.ObserveCronRun(ctx, in)
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				_, err = store.CronRun(ctx, project.ID, in.UID)
				require.ErrorIs(t, err, db.ErrNotFound)
				after, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			in.EventFeatures = db.CronWorkflowArgvEventFeature
			created, err := store.ObserveCronRun(ctx, in)
			require.NoError(t, err)
			in.EventFeatures = ""
			replay, err := store.ObserveCronRun(ctx, in)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			in.Status, in.ExpectedRevision = "succeeded", created.Run.Revision
			_, err = store.ObserveCronRun(ctx, in)
			require.NoError(t, err)
			in.UID, in.ExpectedRevision = newID(), 0
			_, err = store.ObserveCronRun(ctx, in)
			require.NoError(t, err)
		})
	}
	return nil
}

package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCronAdoptionRejectsNonportableTimezone(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{
		ProjectID: project.ID,
		Name:      "Review job",
		Actor:     "operator",
		Definition: cron.JobDefinition{
			Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"},
			Action:  cron.Action{Kind: "execute", Prompt: "Review open work"},
			Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Review"},
			Overlap: "forbid", Catchup: "skip",
		},
	})
	require.NoError(t, err)
	job = storeLegacyLocalTimezone(t, store, job, true)
	maxEventID, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)

	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 42,
		HubProjectUID: hubUID, Actor: "operator",
	})
	require.ErrorIs(t, err, cron.ErrInvalid)
	require.ErrorContains(t, err, `timezone "Local"`)

	unchanged, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, project.UID, unchanged.UID)
	_, err = store.FederationBindingByProject(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	unchangedJob, err := store.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, "Local", unchangedJob.Definition.Trigger.Timezone)
	unchangedMaxEventID, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, maxEventID, unchangedMaxEventID, "failed adoption must not queue snapshots or change event history")
	return nil
}

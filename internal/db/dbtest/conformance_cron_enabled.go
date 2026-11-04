package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Enabled-only changes retain deleted native parents and their progress;
// definition edits still require a live dependency.
func checkCronEnabledDeletedParent(t *testing.T, s db.Storage) error {
	p, parent, job, progress := recurringFixture(t, s, "all")
	parent, _, _, err := s.SoftDeleteIssue(t.Context(), parent.ID, "operator")
	require.NoError(t, err)
	originalMetadata := parent.Metadata
	for _, enabled := range []bool{false, true} {
		def := job.Definition
		def.Enabled = enabled
		current, events, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: def, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		job = current
		out, err := s.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(4 * time.Hour)})
		require.NoError(t, err)
		require.Empty(t, out.Events)
	}
	changed := job.Definition
	policy := *changed.Issue
	policy.Title = "Changed child"
	changed.Issue = &policy
	_, _, err = s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: changed, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
	require.Error(t, err, "other definition changes still require a live parent")
	parent, _, _, err = s.RestoreIssue(t.Context(), parent.ID, "operator")
	require.NoError(t, err)
	require.Equal(t, originalMetadata, parent.Metadata)
	out, err := s.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(4 * time.Hour)})
	require.NoError(t, err)
	require.NotEmpty(t, out.Events)
	t.Run("adapter", func(t *testing.T) {
		project, err := s.CreateProject(t.Context(), "adapter-project")
		require.NoError(t, err)
		parent, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Adapter dependency", Author: "operator"})
		require.NoError(t, err)
		definition := cron.JobDefinition{
			Version: 1, Kind: "job", Enabled: true,
			Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200},
			Action:  cron.Action{Kind: "notify", Recipient: "operator", Message: "Review service"},
			Issue:   &cron.IssuePolicy{Kind: "existing", UID: parent.UID},
			Overlap: "allow", Catchup: "latest",
		}
		job, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Adapter job", Actor: "operator", Definition: definition})
		require.NoError(t, err)
		_, _, _, err = s.SoftDeleteIssue(t.Context(), parent.ID, "operator")
		require.NoError(t, err)
		definition.Enabled = false
		paused, events, err := s.PutCronJob(t.Context(), db.PutCronJob{
			ProjectID: project.ID, UID: job.UID, Name: job.Name, Definition: definition,
			Actor: "operator", ExpectedEventUID: job.DefinitionEventUID,
		})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, definition, paused.Definition)
	})
	return nil
}

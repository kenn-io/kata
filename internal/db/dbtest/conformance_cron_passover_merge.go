package dbtest

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Contract: merge keeps target metadata. Source-only cutoffs that have not
// reached native progress are deliberately lost, with no lazy time bound.
func checkCronPassoverMerge(t *testing.T, _ db.Storage, b Backend) error {
	for _, mode := range []string{"disabled", "closed parent", "persisted progress"} {
		t.Run(mode, func(t *testing.T) {
			s := b.Open(t)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			p, parent, job, progress := recurringFixture(t, s, "all")
			cutoff := progress.Anchor.Add(6 * time.Hour)
			seedPassover(t, s, p.ID, map[string]jsontext.Value{"cron.passover": passoverFixtureValue(t, cutoff)})
			if mode == "disabled" {
				def := job.Definition
				def.Enabled = false
				var err error
				job, _, err = s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Actor: "operator", Definition: def})
				require.NoError(t, err)
			}
			if mode == "closed parent" {
				_, _, _, err := s.CloseIssue(t.Context(), parent.ID, "done", "operator", "Commitment is paused", nil)
				require.NoError(t, err)
			}
			_, err := s.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: cutoff})
			require.NoError(t, err)
			before := fixtureProgress(t, s, parent.ID, job.UID)
			if mode == "persisted progress" {
				require.Equal(t, cutoff, before.Through)
			} else {
				require.Equal(t, progress.Through, before.Through)
			}
			target, err := s.CreateProject(t.Context(), "merge-target")
			require.NoError(t, err)
			_, err = s.MergeProjects(t.Context(), db.MergeProjectsParams{SourceProjectID: p.ID, TargetProjectID: target.ID, Actor: "operator"})
			require.NoError(t, err)
			target, err = s.ProjectByID(t.Context(), target.ID)
			require.NoError(t, err)
			require.Nil(t, db.CronPassoverFromMetadata(target.Metadata, job.UID))
			if mode == "disabled" {
				resumed, err := s.ResumeCronJob(t.Context(), db.ResumeCronJobIn{ProjectID: target.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
				require.NoError(t, err)
				job = resumed.Job
			}
			if mode == "closed parent" {
				_, _, _, err := s.ReopenIssue(t.Context(), parent.ID, "operator")
				require.NoError(t, err)
			}
			_, err = s.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: target.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: cutoff})
			require.NoError(t, err)
			children, err := s.ChildrenOfIssue(t.Context(), parent.ID)
			require.NoError(t, err)
			want := 3
			if mode == "persisted progress" {
				want = 0
			}
			require.Len(t, children, want)
		})
	}
	return nil
}

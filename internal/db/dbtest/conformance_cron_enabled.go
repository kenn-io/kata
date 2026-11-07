package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
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

	}
	changed := job.Definition
	changed.Enabled = !changed.Enabled
	policy := *changed.Issue
	policy.Title = "Changed child"
	changed.Issue = &policy
	_, _, err = s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, Name: job.Name, Definition: changed, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
	require.ErrorIs(t, err, cron.ErrInvalid, "enabled plus other definition changes still require a live parent")
	parent, _, _, err = s.RestoreIssue(t.Context(), parent.ID, "operator")
	require.NoError(t, err)
	require.Equal(t, originalMetadata, parent.Metadata)
	out, err := s.DispatchRecurringCron(t.Context(), db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(4 * time.Hour)})
	require.NoError(t, err)
	require.NotEmpty(t, out.Events)
	return nil
}

// Adapter dependency failures are tested separately from native parent gating.
func checkCronEnabledAdapterTarget(t *testing.T, s db.Storage) error {
	for _, kind := range []string{"deleted", "moved-resume", "moved-pause", "missing-pause"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			project, err := s.CreateProject(ctx, "adapter-"+kind)
			require.NoError(t, err)
			target, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Adapter dependency", Author: "operator"})
			require.NoError(t, err)
			definition := adapterNotifyDefinition(target.UID)
			job, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Adapter job", Actor: "operator", Definition: definition})
			require.NoError(t, err)
			moveTarget := func() {
				other, err := s.CreateProject(ctx, "dependency-"+kind)
				require.NoError(t, err)
				_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: target.ID, FromProjectID: project.ID, ToProjectID: other.ID, IfMatchRev: target.Revision, Actor: "operator"})
				require.NoError(t, err)
				moved, err := s.IssueByUID(ctx, target.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, other.ID, moved.ProjectID)
			}
			if kind == "moved-pause" {
				moveTarget()
			}
			if kind == "missing-pause" {
				// A restored definition may reference a missing issue. The
				// enabled-only exception must still check existence.
				q := s.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				})
				definition.Issue.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
				raw, e := json.Marshal(definition)
				require.NoError(t, e)
				_, err = q.ExecContext(ctx, "UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2", string(raw), job.UID)
				require.NoError(t, err)
				job.Definition = definition
			}
			if kind == "deleted" {
				_, _, _, err = s.SoftDeleteIssue(ctx, target.ID, "operator")
				require.NoError(t, err)
			}
			definition.Enabled = false
			paused, events, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, Definition: definition, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID})
			if kind == "moved-pause" || kind == "missing-pause" {
				require.ErrorIs(t, err, cron.ErrInvalid)
				if kind == "moved-pause" {
					require.ErrorContains(t, err, "dependency belongs to another project")
				}
				require.Empty(t, events)
				current, readErr := s.CronJob(ctx, project.ID, job.UID)
				require.NoError(t, readErr)
				require.Equal(t, job, current, "a refused pause must leave the definition and revision unchanged")
				return
			}
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, definition, paused.Definition)
			if kind == "moved-resume" {
				moveTarget()
				beforeProject, e := s.ProjectByID(ctx, project.ID)
				require.NoError(t, e)
				beforeEvent, e := s.MaxEventID(ctx)
				require.NoError(t, e)
				_, e = s.ResumeCronJob(ctx, db.ResumeCronJobIn{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: paused.DefinitionEventUID, Actor: "operator", DiscardMissed: true})
				require.ErrorIs(t, e, cron.ErrInvalid)
				afterProject, e := s.ProjectByID(ctx, project.ID)
				require.NoError(t, e)
				require.Equal(t, beforeProject, afterProject, "refused discard cannot retain a cutoff")
				afterEvent, e := s.MaxEventID(ctx)
				require.NoError(t, e)
				require.Equal(t, beforeEvent, afterEvent)
			}
			definition.Enabled = true
			_, events, err = s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, Definition: definition, Actor: "operator", ExpectedEventUID: paused.DefinitionEventUID})
			require.ErrorIs(t, err, cron.ErrInvalid)
			if kind == "moved-resume" {
				require.ErrorContains(t, err, "dependency belongs to another project")
			}
			require.Empty(t, events)
			current, err := s.CronJob(ctx, project.ID, job.UID)
			require.NoError(t, err)
			require.Equal(t, paused, current, "a refused resume must leave the definition and revision unchanged")
			if kind == "deleted" {
				_, _, _, err = s.RestoreIssue(ctx, target.ID, "operator")
				require.NoError(t, err)
				resumed, events, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, Definition: definition, Actor: "operator", ExpectedEventUID: paused.DefinitionEventUID})
				require.NoError(t, err)
				require.Len(t, events, 1)
				require.Equal(t, definition, resumed.Definition)
			}
		})
	}
	return nil
}

func adapterNotifyDefinition(targetUID string) cron.JobDefinition {
	return cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "operator", Message: "Review service"}, Issue: &cron.IssuePolicy{Kind: "existing", UID: targetUID}, Overlap: "allow", Catchup: "latest"}
}

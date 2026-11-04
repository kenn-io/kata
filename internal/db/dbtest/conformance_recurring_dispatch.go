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

// Diagnostic fields describe committed children and finalized discarded work,
// including no-op gates; progress metadata events are not deliveries.
func checkRecurringDispatchDecisions(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, parent, job, progress := recurringFixture(t, s, "all")
	in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor}
	check := func(want string, delivered int) db.DispatchRecurringCronOut {
		t.Helper()
		// Revision races have no explain equivalent. Every live gate is read
		// immediately before dispatch on the identical project, job and clock.
		var explanation db.RecurringExplanation
		if want != "definition_changed" {
			var err error
			explanation, err = s.ExplainRecurringCron(ctx, in.ProjectID, in.JobUID, in.Now)
			require.NoError(t, err)
		}
		out, e := s.DispatchRecurringCron(ctx, in)
		require.NoError(t, e)
		require.Equal(t, want, string(out.Reason))
		if want != "definition_changed" {
			expected := out.Reason
			if expected == cron.DispatchDelivered {
				expected = cron.DispatchDue
			}
			require.Equal(t, expected, explanation.Reason)
		}
		require.Equal(t, delivered, out.Delivered)
		return out
	}
	check("idle", 0)
	_, _, _, e := s.CloseIssue(ctx, parent.ID, "done", "operator", "Pause recurring delivery for this commitment", nil)
	require.NoError(t, e)
	check("parent_closed", 0)
	_, _, _, e = s.ReopenIssue(ctx, parent.ID, "operator")
	require.NoError(t, e)
	in.Now = progress.Anchor.Add(4 * time.Hour)
	out := check("delivered", 2)
	require.Len(t, out.Events, 3)
	require.Zero(t, out.Skipped.Count)
	job.Definition.Catchup, job.Definition.GraceSeconds = "skip", 1
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
	require.NoError(t, e)
	in.ExpectedEventUID = job.DefinitionEventUID
	in.Now = progress.Anchor.Add(6*time.Hour + time.Minute)
	require.Positive(t, check("skipped", 0).Skipped.Count)
	in.ExpectedEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	check("definition_changed", 0)
	in.ExpectedEventUID = job.DefinitionEventUID
	in.JobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	check("job_missing", 0)
	in.JobUID = job.UID
	job.Definition.Enabled = false
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator"})
	require.NoError(t, e)
	in.ExpectedEventUID = job.DefinitionEventUID
	check("disabled", 0)
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "operator", Deleted: true})
	require.NoError(t, e)
	in.ExpectedEventUID = job.DefinitionEventUID
	check("deleted", 0)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	// Corrupt restore fixtures exercise fail-closed stage classification. None of
	// these diagnostic failures may commit tentative progress or children.
	job.Definition.Enabled = true
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Corrupt-state review", Definition: job.Definition, Actor: "operator"})
	require.NoError(t, e)
	in.JobUID, in.ExpectedEventUID = job.UID, job.DefinitionEventUID
	parent, e = s.IssueByID(ctx, parent.ID)
	require.NoError(t, e)
	metadata := parent.Metadata
	fail := func(reason string) {
		t.Helper()
		before, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		explanation, err := s.ExplainRecurringCron(ctx, in.ProjectID, in.JobUID, in.Now)
		require.Error(t, err)
		require.Equal(t, cron.DispatchReason(reason), explanation.Reason)
		require.Equal(t, explanation.Reason.PublicDetail(), explanation.Detail)
		out, err := s.DispatchRecurringCron(ctx, in)
		require.Error(t, err)
		require.Equal(t, reason, string(out.Reason))
		require.Empty(t, out.Events)
		require.Zero(t, out.Delivered)
		require.Zero(t, out.Skipped.Count)
		after, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	_, e = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, `{"version":1,"local-sensitive-detail":true}`, job.UID)
	require.NoError(t, e)
	fail("schedule_invalid")
	_, e = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(mustRecurringEnabled(t, job)), job.UID)
	require.NoError(t, e)
	_, e = q.ExecContext(ctx, `UPDATE issues SET metadata='{}' WHERE id=$1`, parent.ID)
	require.NoError(t, e)
	fail("progress_invalid")
	_, e = q.ExecContext(ctx, `UPDATE issues SET metadata=$1,owner=$2 WHERE id=$3`, metadata, "bad\nrecipient", parent.ID)
	require.NoError(t, e)
	in.Now = in.Now.Add(2 * time.Hour)
	fail("recipient_invalid")
	_, e = q.ExecContext(ctx, `UPDATE issues SET owner=NULL,deleted_at=$1 WHERE id=$2`, in.Now.Format(time.RFC3339Nano), parent.ID)
	require.NoError(t, e)
	check("parent_deleted", 0)
	_, e = q.ExecContext(ctx, `UPDATE issues SET deleted_at=NULL WHERE id=$1`, parent.ID)
	require.NoError(t, e)
	_, e = s.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, e)
	_, e = q.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=$1`, p.ID)
	require.NoError(t, e)
	check("binding_disabled", 0)
	_, e = q.ExecContext(ctx, `UPDATE federation_bindings SET role='spoke',hub_project_id=1,hub_url='https://daemon.example' WHERE project_id=$1`, p.ID)
	require.NoError(t, e)
	check("replica", 0)
	in.ProjectID = -1
	check("project_unavailable", 0)
	return nil
}

func mustRecurringEnabled(t *testing.T, job db.CronJob) []byte {
	t.Helper()
	job.Definition.Enabled = true
	raw, e := json.Marshal(job.Definition)
	require.NoError(t, e)
	return raw
}

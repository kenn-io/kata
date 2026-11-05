package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Live explanations share gate precedence, but only report eligibility. Every
// read leaves parent progress/revision and the event cursor untouched.
// Keep these read-only states separate from agreement dispatches: dispatch
// advances progress, so it would change the later idle-recipient and scan cases.
func checkRecurringExplain(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	p, parent, job, progress := recurringFixture(t, s, "all")
	now := progress.Anchor
	check := func(reason string, fails bool) db.RecurringExplanation {
		t.Helper()
		before, e := s.MaxEventID(ctx)
		require.NoError(t, e)
		original, e := s.IssueByID(ctx, parent.ID)
		require.NoError(t, e)
		// Read-only transactions must not invoke mutation fences or actor policy.
		readCtx := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return errors.New("write fence must not run") })
		out, e := s.ExplainRecurringCron(readCtx, p.ID, job.UID, now)
		if fails {
			require.Error(t, e)
		} else {
			require.NoError(t, e)
		}
		require.Equal(t, reason, string(out.Reason))
		require.Equal(t, out.Reason.PublicDetail(), out.Detail)
		after, e := s.MaxEventID(ctx)
		require.NoError(t, e)
		require.Equal(t, before, after)
		current, e := s.IssueByID(ctx, parent.ID)
		require.NoError(t, e)
		require.Equal(t, original.Revision, current.Revision)
		require.Equal(t, original.Metadata, current.Metadata)
		return out
	}
	idle := check("idle", false)
	lower, lowerErr := s.ExplainRecurringCron(ctx, p.ID, strings.ToLower(job.UID), now)
	require.NoError(t, lowerErr)
	require.Equal(t, idle, lower)
	require.NotNil(t, idle.NextDueAt)
	require.Equal(t, progress.Anchor.Add(2*time.Hour), *idle.NextDueAt)
	now = progress.Anchor.Add(4 * time.Hour)
	check("due", false)
	_, _, _, e := s.CloseIssue(ctx, parent.ID, "done", "operator", "Pause this recurring commitment before delivery", nil)
	require.NoError(t, e)
	check("parent_closed", false)
	_, _, _, e = s.ReopenIssue(ctx, parent.ID, "operator")
	require.NoError(t, e)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, e = q.ExecContext(ctx, `UPDATE issues SET metadata='{}' WHERE id=$1`, parent.ID)
	require.NoError(t, e)
	check("progress_invalid", true)
	_, e = q.ExecContext(ctx, `UPDATE issues SET metadata=$1,owner=$2 WHERE id=$3`, parent.Metadata, "bad\nrecipient", parent.ID)
	require.NoError(t, e)
	now = progress.Anchor // recipient validation also applies to otherwise idle jobs.
	check("recipient_invalid", true)
	_, e = q.ExecContext(ctx, `UPDATE issues SET owner=NULL WHERE id=$1`, parent.ID)
	require.NoError(t, e)
	job.Definition.Catchup = "skip"
	job.Definition.GraceSeconds = 1
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID, Definition: job.Definition})
	require.NoError(t, e)
	now = progress.Anchor.Add(3 * time.Hour)
	check("skipped", false)
	job.Definition.Catchup = "latest"
	job.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "* * * * *"}
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID, Definition: job.Definition})
	require.NoError(t, e)
	now = progress.Anchor.Add(1501 * time.Minute)
	check("scanning", false)
	job.Definition.Enabled = false
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID, Definition: job.Definition})
	require.NoError(t, e)
	check("disabled", false)
	job, _, e = s.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: p.ID, Name: job.Name, Actor: "operator", ExpectedEventUID: job.DefinitionEventUID, Definition: job.Definition, Deleted: true})
	require.NoError(t, e)
	check("deleted", false)
	adapter, _, e := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Adapter inspection", Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspection"}, Overlap: "forbid", Catchup: "all"}})
	require.NoError(t, e)
	native := job
	job = adapter
	check("external_adapter", false)
	job = native
	_, e = s.EnableProjectFederation(ctx, p.ID, "operator")
	require.NoError(t, e)
	_, e = q.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=$1`, p.ID)
	require.NoError(t, e)
	check("binding_disabled", false)
	_, e = q.ExecContext(ctx, `UPDATE federation_bindings SET role='spoke',hub_project_id=1,hub_url='https://daemon.example' WHERE project_id=$1`, p.ID)
	require.NoError(t, e)
	check("replica", false)
	missing, e := s.ExplainRecurringCron(ctx, p.ID, "01K5ZN7SP700000000000000001", now)
	require.NoError(t, e)
	require.Equal(t, cron.DispatchJobMissing, missing.Reason, "explanation checks membership inside its read snapshot before early authority gates")
	out, e := s.ExplainRecurringCron(ctx, -1, job.UID, now)
	require.NoError(t, e)
	require.Equal(t, "project_unavailable", string(out.Reason))
	return nil
}

func checkRecurringExplainArchivedBinding(t *testing.T, s db.Storage) error {
	p, _, job, progress := recurringFixture(t, s, "all")
	_, err := s.EnableProjectFederation(t.Context(), p.ID, "operator")
	require.NoError(t, err)
	_, _, err = s.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: p.ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for _, role := range []string{"spoke", "hub"} {
		t.Run(role, func(t *testing.T) {
			_, err := q.ExecContext(t.Context(), `UPDATE federation_bindings SET role=$1,enabled=0,hub_project_id=CASE WHEN $1='spoke' THEN 1 ELSE 0 END,hub_url=CASE WHEN $1='spoke' THEN 'https://daemon.example' ELSE '' END WHERE project_id=$2`, role, p.ID)
			require.NoError(t, err)
			out, err := s.ExplainRecurringCron(t.Context(), p.ID, job.UID, progress.Anchor)
			require.NoError(t, err)
			require.Equal(t, cron.DispatchProjectUnavailable, out.Reason)
		})
	}
	return nil
}

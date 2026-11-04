package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// The fleet contract is an intersection before limiting; unchanged data pages
// must equal an independently filtered and sorted reference model.
func checkCronFleetReads(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	projects := make([]db.Project, 3)
	jobs := make([]db.CronJob, 3)
	var records []db.CronRun
	var issueUIDs []string
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	states := []string{"running", "succeeded", "failed", "cancelled", "unknown"}
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for p := range projects {
		var err error
		projects[p], err = store.CreateProject(ctx, fmt.Sprintf("example-project-%d", p))
		require.NoError(t, err)
		def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
		require.NoError(t, err)
		jobs[p], _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: projects[p].ID, Name: "Review", Actor: "operator", Definition: def})
		require.NoError(t, err)
		issues := make([]db.Issue, 2)
		for i := range issues {
			issues[i], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[p].ID, Title: fmt.Sprintf("Task %d", i), Author: "operator"})
			require.NoError(t, err)
			issueUIDs = append(issueUIDs, issues[i].UID)
		}
		for n := range 10 {
			id, err := uid.New()
			require.NoError(t, err)
			issue := issues[n%2].UID
			label := fmt.Sprintf("executor-%d", n%3)
			teammate := fmt.Sprintf("runner-%d", n%2)
			input := db.ObserveCronRun{ProjectID: projects[p].ID, UID: id, JobUID: &jobs[p].UID, DefinitionEventUID: &jobs[p].DefinitionEventUID, IssueUID: &issue, ExecutorLabel: &label, Teammate: &teammate, Actor: "operator", Status: states[n%len(states)], Summary: cron.Summary{Version: 1}}
			if n%4 == 0 {
				input.IssueUID = nil
			}
			if n%4 == 1 {
				input.ExecutorLabel = nil
			}
			if n%4 == 2 {
				input.Teammate = nil
			}
			result, err := store.ObserveCronRun(ctx, input)
			require.NoError(t, err)
			result.Run.CreatedAt = base.Add(time.Duration(n/2) * time.Second)
			_, err = q.ExecContext(ctx, "UPDATE cron_runs SET created_at=$1 WHERE uid=$2", result.Run.CreatedAt.Format(db.CronRunCreatedAtFormat), id)
			require.NoError(t, err)
			records = append(records, result.Run)
		}
	}
	if len(records) == 0 {
		return fmt.Errorf("cron fleet fixture has no run records")
	}
	if len(issueUIDs) == 0 {
		return fmt.Errorf("cron fleet fixture has no issue UIDs")
	}
	_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: projects[2].ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: projects[0].ID, UID: jobs[0].UID, Name: jobs[0].Name, Definition: jobs[0].Definition, Actor: "operator", ExpectedEventUID: jobs[0].DefinitionEventUID, Deleted: true})
	require.NoError(t, err)
	slices.SortFunc(records, func(a, b db.CronRun) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.UID, a.UID)
	})
	hegel.Test(t, func(ht *hegel.T) {
		in := db.CronRunList{AllProjects: true, Limit: hegel.Draw(ht, hegel.Integers(1, 100))}
		if hegel.Draw(ht, hegel.Booleans()) {
			in.Statuses = []string{hegel.Draw(ht, hegel.SampledFrom(states)), hegel.Draw(ht, hegel.SampledFrom(states))}
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			in.IssueUID = hegel.Draw(ht, hegel.SampledFrom(issueUIDs))
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			in.ExecutorLabel = fmt.Sprintf("executor-%d", hegel.Draw(ht, hegel.Integers(0, 3)))
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			in.Teammate = fmt.Sprintf("runner-%d", hegel.Draw(ht, hegel.Integers(0, 2)))
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			in.Since = base.Add(time.Duration(hegel.Draw(ht, hegel.Integers(0, 5))) * time.Second)
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			in.ProjectIDs = []int64{projects[hegel.Draw(ht, hegel.Integers(0, 2))].ID}
		}
		readCtx := ctx
		var allowed []string
		if hegel.Draw(ht, hegel.Booleans()) {
			allowed = []string{}
			if hegel.Draw(ht, hegel.Booleans()) {
				allowed = append(allowed, projects[0].UID)
			}
			readCtx = db.WithAuthorizedProjects(ctx, allowed)
		}
		want := []string{}
		for _, run := range records {
			if run.ProjectID == projects[2].ID {
				continue
			}
			if len(in.ProjectIDs) > 0 && !slices.Contains(in.ProjectIDs, run.ProjectID) {
				continue
			}
			if allowed != nil {
				projectUID := projects[0].UID
				if run.ProjectID == projects[1].ID {
					projectUID = projects[1].UID
				}
				if !slices.Contains(allowed, projectUID) {
					continue
				}
			}
			if len(in.Statuses) > 0 && !slices.Contains(in.Statuses, run.Status) {
				continue
			}
			if in.IssueUID != "" && (run.IssueUID == nil || *run.IssueUID != in.IssueUID) {
				continue
			}
			if in.ExecutorLabel != "" && (run.ExecutorLabel == nil || *run.ExecutorLabel != in.ExecutorLabel) {
				continue
			}
			if in.Teammate != "" && (run.Teammate == nil || *run.Teammate != in.Teammate) {
				continue
			}
			if !in.Since.IsZero() && run.CreatedAt.Before(in.Since) {
				continue
			}
			want = append(want, run.UID)
		}
		got := []string{}
		for pages := 0; pages <= len(records); pages++ {
			page, err := store.ListCronRuns(readCtx, in)
			require.NoError(ht, err)
			for _, run := range page {
				got = append(got, run.UID)
			}
			if len(page) < in.Limit {
				break
			}
			in.BeforeUID = page[len(page)-1].UID
		}
		require.Equal(ht, want, got)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
	for _, p := range projects[:2] {
		scoped, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: p.ID})
		require.NoError(t, err)
		global, err := store.ListCronRuns(ctx, db.CronRunList{AllProjects: true, ProjectIDs: []int64{p.ID}})
		require.NoError(t, err)
		require.Equal(t, scoped, global)
	}
	// A visible anchor need not match the filters on the requested page.
	anchor := records[0]
	for _, record := range records {
		if record.ProjectID == projects[0].ID && record.Status == "succeeded" {
			anchor = record
			break
		}
	}
	page, err := store.ListCronRuns(ctx, db.CronRunList{AllProjects: true, BeforeUID: anchor.UID, Statuses: []string{"failed"}})
	require.NoError(t, err)
	wantIDs := []string{}
	for _, record := range records {
		if record.ProjectID == projects[2].ID || record.Status != "failed" {
			continue
		}
		if record.CreatedAt.Before(anchor.CreatedAt) || (record.CreatedAt.Equal(anchor.CreatedAt) && record.UID < anchor.UID) {
			wantIDs = append(wantIDs, record.UID)
		}
	}
	gotIDs := []string{}
	for _, record := range page {
		gotIDs = append(gotIDs, record.UID)
	}
	require.Equal(t, wantIDs, gotIDs)

	for _, in := range []db.CronRunList{
		{AllProjects: true, Statuses: []string{"bogus"}}, {AllProjects: true, IssueUID: "bad"},
		{AllProjects: true, ExecutorLabel: " "}, {AllProjects: true, Teammate: "bad/name"},
		{AllProjects: true, BeforeUID: records[0].UID, ProjectIDs: []int64{projects[0].ID}},
		{AllProjects: true, BeforeUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ"},
	} {
		// The first sorted row belongs to the archived project.
		_, err := store.ListCronRuns(ctx, in)
		require.ErrorIs(t, err, cron.ErrInvalid)
	}
	for _, include := range []bool{false, true} {
		got, err := store.ListAllCronJobs(ctx, db.CronAllList{IncludeDeleted: include})
		require.NoError(t, err)
		count := 1
		if include {
			count = 2
		}
		require.Len(t, got, count)
		for _, job := range got {
			require.NotEqual(t, projects[2].ID, job.ProjectID)
		}
	}
	// Retained run evidence can still be selected by a removed issue's UID.
	issue, err := store.IssueByUID(ctx, issueUIDs[0], db.IncludeDeletedNo)
	require.NoError(t, err)
	before, err := store.ListCronRuns(ctx, db.CronRunList{AllProjects: true, IssueUID: issue.UID})
	require.NoError(t, err)
	require.NotEmpty(t, before)
	_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "operator")
	require.NoError(t, err)
	after, err := store.ListCronRuns(ctx, db.CronRunList{AllProjects: true, IssueUID: strings.ToLower(issue.UID)})
	require.NoError(t, err)
	require.Equal(t, before, after)

	hidden, err := store.ListAllCronJobs(db.WithAuthorizedProjects(ctx, []string{}), db.CronAllList{})
	require.NoError(t, err)
	require.Empty(t, hidden)
	return nil
}

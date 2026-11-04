package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

type cronTallyFixture struct {
	store    db.Storage
	project  db.Project
	job      db.CronJob
	workflow db.CronWorkflow
	before   time.Time
	q        interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}
}

func newCronTallyFixture(ctx context.Context, t require.TestingT, store db.Storage) cronTallyFixture {
	id, err := uid.New()
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "tally-"+id)
	require.NoError(t, err)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{UID: id, ProjectID: project.ID, Name: "Review", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: id, ProjectID: project.ID, Name: "Workflow", Actor: "operator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "review", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	return cronTallyFixture{store: store, project: project, job: job, workflow: workflow, before: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), q: store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})}
}

func (f cronTallyFixture) add(ctx context.Context, t require.TestingT, kind, status string, created time.Time, id string) db.CronRun {
	if id == "" {
		var err error
		id, err = uid.New()
		require.NoError(t, err)
	}
	in := db.ObserveCronRun{UID: id, ProjectID: f.project.ID, Actor: "operator", Status: status, Summary: cron.Summary{Version: 1}, WorkflowUID: &f.workflow.UID, WorkflowDefinitionEventUID: &f.workflow.DefinitionEventUID}
	if kind == "job" {
		in.JobUID, in.DefinitionEventUID = &f.job.UID, &f.job.DefinitionEventUID
	}
	result, err := f.store.ObserveCronRun(ctx, in)
	require.NoError(t, err)
	_, err = f.q.ExecContext(ctx, `UPDATE cron_runs SET created_at=$1,updated_at=$2 WHERE uid=$3`, created.Format(db.CronRunCreatedAtFormat), created.Format(time.RFC3339Nano), id)
	require.NoError(t, err)
	return result.Run
}

func (f cronTallyFixture) seed(ctx context.Context, t require.TestingT, entry db.CronTallyEntry) {
	value := db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{entry.Kind: entry}}
	raw, err := json.Marshal(map[string]any{db.CronTallyPrefix + entry.UID: value, "ordinary": "preserved"}, json.Deterministic(true))
	require.NoError(t, err)
	_, err = f.q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), f.project.ID)
	require.NoError(t, err)
}

// Contract: final selected statuses accumulate once in the prune transaction;
// previews, empty batches and failed transactions leave the entire state alone.
func checkCronRunTally(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f := newCronTallyFixture(ctx, t, store)
	old := f.before.Add(-time.Hour)
	first := f.add(ctx, t, "job", "succeeded", old, "")
	f.add(ctx, t, "job", "failed", old.Add(time.Second), "")
	f.add(ctx, t, "workflow", "unknown", old.Add(2*time.Second), "")
	for _, kind := range []string{"job", "workflow"} {
		f.add(ctx, t, kind, "running", f.before.Add(-time.Minute), "")
	}
	before, err := store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	in := db.PruneCronRuns{ProjectID: f.project.ID, Actor: "operator", Before: f.before, Keep: 1, DryRun: true}
	preview, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Equal(t, 3, preview.Pruned)
	require.Zero(t, preview.MetadataEvent.ID)
	project, err := store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	require.Equal(t, before.Metadata, project.Metadata)
	require.Equal(t, before.Revision, project.Revision)
	in.DryRun = false
	pruned, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "project.metadata_updated", pruned.MetadataEvent.Type)
	require.Equal(t, "operator", pruned.MetadataEvent.Actor)
	require.Greater(t, pruned.ResetAfterEventID, pruned.MetadataEvent.ID)
	project, err = store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	require.Equal(t, before.Revision+1, project.Revision)
	counts, err := store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID})
	require.NoError(t, err)
	require.Len(t, counts.Groups, 2)
	require.Equal(t, db.CronTallyCounts{Succeeded: 1, Failed: 1}, counts.Groups[0].PrunedSinceTracking)
	require.Equal(t, db.CronTallyCounts{Unknown: 1}, counts.Groups[1].PrunedSinceTracking)
	for _, group := range counts.Groups {
		require.Equal(t, db.CronRunRetainedCounts{Running: 1}, group.Retained)
		require.NotNil(t, group.CoverageStartedAt)
	}
	empty, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Zero(t, empty.MetadataEvent.ID)
	afterEmpty, err := store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	require.Equal(t, project, afterEmpty)
	// The observation dedup evidence was pruned. Reaccepting the UID and
	// pruning a different final status records another action, not a distinct UID.
	f.add(ctx, t, "job", "failed", old, first.UID)
	_, err = store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: f.job.UID})
	require.NoError(t, err)
	require.Equal(t, db.CronTallyCounts{Succeeded: 1, Failed: 2}, counts.Groups[0].PrunedSinceTracking)
	// A backward wall clock cannot invert coverage or last-prune timestamps.
	project, err = store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	values, err := db.DecodeCronTallies(project.Metadata)
	require.NoError(t, err)
	entry := values[f.job.UID].Groups["job"]
	entry.LastPrunedAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	f.seed(ctx, t, entry)
	f.add(ctx, t, "job", "cancelled", old, "")
	_, err = store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: f.job.UID})
	require.NoError(t, err)
	require.True(t, counts.Groups[0].LastPrunedAt.Equal(entry.LastPrunedAt))
	require.True(t, counts.Groups[0].CoverageStartedAt.Equal(entry.CoverageStartedAt))
	// Overflow occurs after event deletion/selection but must roll it all back.
	entry.Counts.Failed = math.MaxInt64
	f.seed(ctx, t, entry)
	overflowRun := f.add(ctx, t, "job", "failed", old, "")
	before, err = store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	eventsBefore, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: f.project.ID})
	require.NoError(t, err)
	resetBefore, err := store.PurgeResetCheck(ctx, 0, f.project.ID)
	require.NoError(t, err)
	_, err = store.PruneCronRuns(ctx, in)
	require.Error(t, err)
	project, err = store.ProjectByID(ctx, f.project.ID)
	require.NoError(t, err)
	require.Equal(t, before, project)
	_, err = store.CronRun(ctx, f.project.ID, overflowRun.UID)
	require.NoError(t, err)
	eventsAfter, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: f.project.ID})
	require.NoError(t, err)
	require.Equal(t, eventsBefore, eventsAfter)
	resetAfter, err := store.PurgeResetCheck(ctx, 0, f.project.ID)
	require.NoError(t, err)
	require.Equal(t, resetBefore, resetAfter)
	return nil
}

// The independent model retains an observation list and action counters. It
// never derives expected counts from production preview/metadata/fold helpers.
func checkCronRunTallyModel(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	hegel.Test(t, func(ht *hegel.T) {
		f := newCronTallyFixture(ctx, ht, store)
		data := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 7)))
		if len(data) > 16 {
			data = data[:16]
		}
		if len(data) == 0 {
			data = []int{0}
		}
		limit := hegel.Draw(ht, hegel.Integers(1, 5))
		statuses := []string{"succeeded", "failed", "cancelled", "unknown"}
		for i, value := range data {
			kind := "job"
			if value >= 4 {
				kind = "workflow"
			}
			f.add(ctx, ht, kind, statuses[value%4], f.before.Add(time.Duration(i-100)*time.Second), "")
		}
		for _, kind := range []string{"job", "workflow"} {
			f.add(ctx, ht, kind, "running", f.before.Add(-time.Second), "")
		}
		actions := [2][4]int64{}
		oldest := [2]*time.Time{}
		newest := [2]*time.Time{}
		for removed := 0; removed < len(data); {
			end := min(removed+limit, len(data))
			for i, value := range data[removed:end] {
				actions[value/4][value%4]++
				at := f.before.Add(time.Duration(removed+i-100) * time.Second)
				if oldest[value/4] == nil {
					oldest[value/4] = &at
				}
				newest[value/4] = &at
			}
			result, err := store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: f.project.ID, Actor: "operator", Before: f.before, Keep: 1, Limit: limit})
			require.NoError(ht, err)
			require.Equal(ht, end-removed, result.Pruned)
			retained := [2][4]int64{}
			for _, value := range data[end:] {
				retained[value/4][value%4]++
			}
			counts, err := store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID})
			require.NoError(ht, err)
			require.Len(ht, counts.Groups, 2)
			for i, group := range counts.Groups {
				require.Equal(ht, [4]int64{group.PrunedSinceTracking.Succeeded, group.PrunedSinceTracking.Failed, group.PrunedSinceTracking.Cancelled, group.PrunedSinceTracking.Unknown}, actions[i])
				require.Equal(ht, db.CronRunRetainedCounts{Running: 1, Succeeded: retained[i][0], Failed: retained[i][1], Cancelled: retained[i][2], Unknown: retained[i][3]}, group.Retained)
				if actions[i] != [4]int64{} {
					require.NotNil(ht, group.CoverageStartedAt)
					require.True(ht, group.OldestCreatedAt.Equal(*oldest[i]))
					require.True(ht, group.NewestCreatedAt.Equal(*newest[i]))
				} else {
					require.Nil(ht, group.CoverageStartedAt)
				}
			}
			removed = end
		}
	}, hegel.WithTestCases(80))
	return nil
}

func checkCronRunCounts(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f := newCronTallyFixture(ctx, t, store)
	counts, err := store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID})
	require.NoError(t, err)
	require.Empty(t, counts.Groups, "definitions without runs/tallies are not enumerated")
	for i, status := range []string{"running", "succeeded", "failed", "cancelled", "unknown"} {
		f.add(ctx, t, "job", status, f.before.Add(time.Duration(i-10)*time.Minute), "")
	}
	f.add(ctx, t, "workflow", "failed", f.before.Add(-time.Minute), "")
	// Workflow membership is not the workflow aggregate identity.
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, WorkflowUID: f.workflow.UID})
	require.NoError(t, err)
	require.Len(t, counts.Groups, 1)
	require.Equal(t, "workflow", counts.Groups[0].Kind)
	require.Equal(t, db.CronRunRetainedCounts{Failed: 1}, counts.Groups[0].Retained)
	require.Nil(t, counts.Groups[0].CoverageStartedAt)
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: f.job.UID})
	require.NoError(t, err)
	require.Equal(t, db.CronRunRetainedCounts{Running: 1, Succeeded: 1, Failed: 1, Cancelled: 1, Unknown: 1}, counts.Groups[0].Retained)
	_, err = f.q.ExecContext(ctx, `UPDATE cron_jobs SET deleted_at=$1 WHERE uid=$2`, f.before.Format(time.RFC3339Nano), f.job.UID)
	require.NoError(t, err)
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: f.job.UID})
	require.NoError(t, err)
	require.Equal(t, "Review", counts.Groups[0].Name, "tombstones still name retained evidence")
	_, err = store.CronRunCounts(db.WithAuthorizedProjects(ctx, []string{}), db.CronRunCountsQuery{ProjectID: f.project.ID})
	require.ErrorIs(t, err, db.ErrNotFound)
	denied := errors.New("project access denied")
	_, err = store.CronRunCounts(db.WithProjectAccessSnapshotCheck(ctx, func(context.Context, db.Transaction, string) error { return denied }), db.CronRunCountsQuery{ProjectID: f.project.ID})
	require.ErrorIs(t, err, denied)
	checkCronRunCountsSnapshotAuthorization(t, store, f.project)
	_, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: f.job.UID, WorkflowUID: f.workflow.UID})
	require.Error(t, err)
	_, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: "invalid"})
	require.Error(t, err)
	missing, err := uid.New()
	require.NoError(t, err)
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: missing})
	require.NoError(t, err)
	require.Empty(t, counts.Groups)
	_, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: -1})
	require.ErrorIs(t, err, db.ErrNotFound)
	entry := db.CronTallyEntry{Kind: "job", UID: missing, CoverageStartedAt: f.before, LastPrunedAt: f.before.Add(time.Hour), OldestCreatedAt: f.before.Add(-time.Hour), NewestCreatedAt: f.before, Counts: db.CronTallyCounts{Succeeded: 9007199254740993}}
	f.seed(ctx, t, entry)
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: missing})
	require.NoError(t, err)
	require.Len(t, counts.Groups, 1)
	require.Empty(t, counts.Groups[0].Name)
	require.Zero(t, counts.Groups[0].Retained)
	require.Equal(t, int64(9007199254740993), counts.Groups[0].PrunedSinceTracking.Succeeded)
	raw, err := json.Marshal(counts)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"succeeded":9007199254740993`)
	// Every generic store patch refuses the prefix even for no-op/null/mixed
	// writes. Tests seed lifecycle state explicitly through SQL instead.
	for _, patch := range []map[string]jsontext.Value{
		{db.CronTallyPrefix + missing: []byte("null")},
		{db.CronTallyPrefix + "bad": []byte("null"), "ordinary": []byte("2")},
		{db.CronTallyPrefix: []byte("{}")},
	} {
		_, err := store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: f.project.ID, Actor: "system", Patch: patch})
		require.Error(t, err)
	}
	// Corruption is never interpreted as untracked zeroes, even for an empty
	// preview. The counts filter cannot conceal corrupt sibling tallies.
	_, err = f.q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, fmt.Sprintf(`{%q:null}`, db.CronTallyPrefix+missing), f.project.ID)
	require.NoError(t, err)
	_, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID, JobUID: f.job.UID})
	require.Error(t, err)
	_, err = store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: f.project.ID, Actor: "operator", Before: f.before.Add(-24 * time.Hour), DryRun: true})
	require.Error(t, err)
	return nil
}

func checkCronRunCountsSnapshotAuthorization(t *testing.T, store db.Storage, project db.Project) {
	t.Helper()
	ctx := t.Context()
	const actor = "cron-counts-reader"
	_, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "cron-counts-reader-token", Actor: actor, AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	team, _, err := store.CreateTeam(ctx, "cron-counts-readers", db.BootstrapActor)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		scope := hegel.Draw(ht, hegel.Integers(0, 2)) // owner, empty, or one admitted project
		public := hegel.Draw(ht, hegel.Booleans())
		member := hegel.Draw(ht, hegel.Booleans())
		validActor := hegel.Draw(ht, hegel.Booleans())
		visibility, teamUIDs := "teams", []string{team.UID}
		if public {
			visibility, teamUIDs = "all", nil
		}
		_, _, err := store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: visibility, TeamUIDs: teamUIDs}, db.BootstrapActor)
		require.NoError(ht, err)
		_, err = store.SetTeamMembership(ctx, team.UID, actor, member, db.BootstrapActor)
		require.NoError(ht, err)

		readCtx := ctx
		switch scope {
		case 0:
			readCtx = db.WithAuthorizedProjects(readCtx, nil)
		case 1:
			readCtx = db.WithAuthorizedProjects(readCtx, []string{})
		case 2:
			readCtx = db.WithAuthorizedProjects(readCtx, []string{project.UID})
			checkActor := actor
			if !validActor {
				checkActor = db.BootstrapActor
			}
			readCtx = db.WithProjectAccessSnapshotCheck(readCtx, func(checkCtx context.Context, tx db.Transaction, projectUID string) error {
				return store.ProjectAccessSnapshotCheck(checkActor, []string{projectUID})(checkCtx, tx)
			})
		}
		_, err = store.CronRunCounts(readCtx, db.CronRunCountsQuery{ProjectID: project.ID})
		allowed := scope == 0 || (scope == 2 && validActor && (public || member))
		if allowed {
			require.NoError(ht, err)
		} else {
			require.ErrorIs(ht, err, db.ErrNotFound)
		}
	}, hegel.WithTestCases(64))
}

func checkCronRunCountsSnapshot(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f := newCronTallyFixture(ctx, t, store)
	f.add(ctx, t, "job", "failed", f.before.Add(-time.Hour), "")
	f.add(ctx, t, "job", "running", f.before.Add(-time.Minute), "")
	// The normal transaction authorization callback sits between reading the
	// metadata document and its retained aggregates. Commit a real writer there
	// to distinguish a stable snapshot from independently consistent queries.
	staged := false
	readCtx := db.WithProjectAccessSnapshotCheck(ctx, func(_ context.Context, _ db.Transaction, projectUID string) error {
		require.Equal(t, f.project.UID, projectUID)
		result, err := store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: f.project.ID, Actor: "operator", Before: f.before, Keep: 1})
		require.NoError(t, err)
		require.Equal(t, 1, result.Pruned)
		staged = true
		return nil
	})
	counts, err := store.CronRunCounts(readCtx, db.CronRunCountsQuery{ProjectID: f.project.ID})
	require.NoError(t, err)
	require.True(t, staged)
	require.Len(t, counts.Groups, 1)
	require.Equal(t, db.CronRunRetainedCounts{Running: 1, Failed: 1}, counts.Groups[0].Retained)
	require.Zero(t, counts.Groups[0].PrunedSinceTracking)
	require.Nil(t, counts.Groups[0].CoverageStartedAt)
	counts, err = store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID})
	require.NoError(t, err)
	require.Equal(t, db.CronRunRetainedCounts{Running: 1}, counts.Groups[0].Retained)
	require.Equal(t, db.CronTallyCounts{Failed: 1}, counts.Groups[0].PrunedSinceTracking)
	return nil
}

func checkCronRunTallyConcurrent(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f := newCronTallyFixture(ctx, t, store)
	for i := range 4 {
		f.add(ctx, t, "job", "succeeded", f.before.Add(time.Duration(i-10)*time.Minute), "")
	}
	f.add(ctx, t, "job", "running", f.before.Add(-time.Minute), "")
	type outcome struct {
		result db.CronRunPruneResult
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: f.project.ID, Actor: "operator", Before: f.before, Keep: 1, Limit: 2})
			results <- outcome{result, err}
		}()
	}
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		require.Equal(t, 2, result.result.Pruned)
	}
	counts, err := store.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: f.project.ID})
	require.NoError(t, err)
	require.Equal(t, db.CronTallyCounts{Succeeded: 4}, counts.Groups[0].PrunedSinceTracking)
	require.Equal(t, db.CronRunRetainedCounts{Running: 1}, counts.Groups[0].Retained)
	return nil
}

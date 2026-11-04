package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// Contract: pruning removes only old terminal evidence beyond each definition's
// newest set; previews and empty retries change neither events nor reset state.
func checkCronRunPrune(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f := newCronTallyFixture(ctx, t, store)
	project, job, workflow, q := f.project, f.job, f.workflow, f.q
	var err error
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 123000000, time.UTC)
	add := func(status string, created, updated time.Time, workflowOnly bool) db.CronRun {
		id, err := uid.New()
		require.NoError(t, err)
		in := db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "operator", Status: status, Summary: cron.Summary{Version: 1}, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID}
		if workflowOnly {
			in.JobUID = nil
			in.DefinitionEventUID = nil
			in.WorkflowUID = &workflow.UID
			in.WorkflowDefinitionEventUID = &workflow.DefinitionEventUID
		}
		result, err := store.ObserveCronRun(ctx, in)
		require.NoError(t, err)
		_, err = q.ExecContext(ctx, `UPDATE cron_runs SET created_at=$1,updated_at=$2 WHERE uid=$3`, created.UTC().Format(db.CronRunCreatedAtFormat), updated.Format(time.RFC3339Nano), id)
		require.NoError(t, err)
		return result.Run
	}
	old := cutoff.Add(-time.Hour)
	removed := add("succeeded", old, old, false)
	recent := add("failed", old.Add(time.Second), cutoff, false)
	boundary := add("unknown", old.Add(2*time.Second), cutoff.Add(-time.Nanosecond), false)
	running := add("running", old.Add(3*time.Second), old, false)
	workflowKept := add("failed", old, old, true)
	in := db.PruneCronRuns{ProjectID: project.ID, Actor: "operator", Before: cutoff, Keep: 1, Limit: 1, DryRun: true}
	for _, outsideUTC := range []time.Time{time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600)), time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("offset", -3600))} {
		invalid := in
		invalid.Before = outsideUTC
		_, err := store.PruneCronRuns(ctx, invalid)
		require.ErrorIs(t, err, cron.ErrInvalid, "UTC-normalized cutoff must stay in years 1..9999")
	}
	maxBefore, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.PruneCronRuns(canceled, in)
	require.ErrorIs(t, err, context.Canceled)
	cursorAfterCancel, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, maxBefore, cursorAfterCancel)
	preview, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Equal(t, 1, preview.Pruned)
	require.True(t, preview.More)
	require.Equal(t, 1, preview.EventsDeleted)
	require.Zero(t, preview.ResetAfterEventID)
	maxAfter, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, maxBefore, maxAfter)
	wantGroups := []db.CronRunPruneGroup{{JobUID: &job.UID, Name: "Review", Counts: map[string]int{"succeeded": 1, "failed": 0, "cancelled": 0, "unknown": 0}, OldestCreatedAt: old, NewestCreatedAt: old}}
	require.Equal(t, wantGroups, preview.Groups)
	// Tombstoned definitions still name the run evidence operators are removing.
	_, err = q.ExecContext(ctx, `UPDATE cron_jobs SET deleted_at=$1 WHERE uid=$2`, cutoff.Format(time.RFC3339Nano), job.UID)
	require.NoError(t, err)
	tombstonedPreview, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Equal(t, wantGroups, tombstonedPreview.Groups)
	in.DryRun = false
	result, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Equal(t, wantGroups, result.Groups)
	require.Equal(t, preview.EventsDeleted, result.EventsDeleted)
	require.Greater(t, result.ResetAfterEventID, maxBefore)
	ui := store.(db.UIStore)
	cursor, err := ui.UIEventCursor(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, cursor, result.ResetAfterEventID)
	snapshot, err := ui.ReadUISnapshot(ctx, db.UISnapshotQuery{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, snapshot.Cursor, result.ResetAfterEventID)

	_, err = store.CronRun(ctx, project.ID, removed.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	reset, err := store.PurgeResetCheck(ctx, maxBefore, project.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, reset, result.ResetAfterEventID)
	second, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Equal(t, 1, second.Pruned)
	require.False(t, second.More)
	_, err = store.CronRun(ctx, project.ID, boundary.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	for _, kept := range []db.CronRun{recent, running, workflowKept} {
		_, err = store.CronRun(ctx, project.ID, kept.UID)
		require.NoError(t, err)
	}
	empty, err := store.PruneCronRuns(ctx, in)
	require.NoError(t, err)
	require.Zero(t, empty.Pruned)
	require.Zero(t, empty.ResetAfterEventID)
	require.False(t, empty.More)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: removed.UID, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "succeeded", Summary: cron.Summary{Version: 1}, ExpectedRevision: 1})
	require.ErrorIs(t, err, db.ErrCronConflict)
	exported := 0
	for _, err := range store.ExportCronRuns(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		exported++
	}
	require.Equal(t, 3, exported)
	_, err = store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, BeforeUID: removed.UID})
	require.ErrorIs(t, err, db.ErrNotFound)
	// Corruption is never interpreted as untracked zeroes, even for an empty
	// preview. Counts exercise the same corrupt sibling document separately.
	_, err = q.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, fmt.Sprintf(`{%q:null}`, db.CronTallyPrefix+job.UID), project.ID)
	require.NoError(t, err)
	_, err = store.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: project.ID, Actor: "operator", Before: cutoff.Add(-24 * time.Hour), DryRun: true})
	require.Error(t, err)

	return nil
}

// Contract: the selected batch agrees with an instant-based reference model,
// with ranking over every status and narrowing applied only after ranking.
func checkCronRunPruneModel(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "model-project")
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 123000000, time.UTC)
	type run struct {
		id, group, status string
		kind              int
		created, updated  time.Time
		job, workflow     *string
	}
	hegel.Test(t, func(ht *hegel.T) {
		cutoff := cutoff.In(time.FixedZone("cutoff", hegel.Draw(ht, hegel.Integers(-12, 14))*3600))
		raw := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 10000)))
		if len(raw) > 24 {
			raw = raw[:24]
		}
		keep := hegel.Draw(ht, hegel.Integers(1, 1000))
		if hegel.Draw(ht, hegel.Booleans()) {
			keep = hegel.Draw(ht, hegel.Integers(1, 4))
		}
		limit := hegel.Draw(ht, hegel.Integers(1, 20000))
		if hegel.Draw(ht, hegel.Booleans()) {
			limit = hegel.Draw(ht, hegel.Integers(1, 4))
		}
		narrow := hegel.Draw(ht, hegel.Integers(0, 4))
		_, err := q.ExecContext(ctx, `DELETE FROM cron_runs WHERE project_id=$1`, project.ID)
		require.NoError(ht, err)
		runs := []run{}
		for i, v := range raw {
			group := fmt.Sprintf("%026d", v%2+1)
			id := fmt.Sprintf("%026d", i+100)
			r := run{id: id, group: group, kind: (v / 2) % 2, status: []string{"running", "succeeded", "failed", "cancelled", "unknown"}[(v/4)%5], created: cutoff.Add(time.Duration((v/20)%7-4) * time.Second), updated: cutoff.Add(time.Duration((v/140)%7-4) * time.Nanosecond)}
			if hegel.Draw(ht, hegel.Booleans()) {
				r.created = cutoff.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-1, 1))))
			}
			// Product creation timestamps are canonical UTC strings.
			r.created = r.created.UTC()
			if r.kind == 0 {
				r.job = &group
			} else {
				r.workflow = &group
			}
			// Runs may carry both references. Job identity has precedence.
			if r.kind == 0 && v%3 == 0 {
				r.workflow = &group
			}
			// Observation, federation materialization and import all persist UTC.
			updated := r.updated.UTC()
			var jobEvent, workflowEvent *string
			event := fmt.Sprintf("%026d", 50)
			if r.job != nil {
				jobEvent = &event
			}
			if r.workflow != nil {
				workflowEvent = &event
			}
			_, err = q.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,workflow_uid,workflow_definition_event_uid,actor,status,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'operator',$7,$8,$9)`, r.id, project.ID, r.job, jobEvent, r.workflow, workflowEvent, r.status, r.created.Format(db.CronRunCreatedAtFormat), updated.Format(time.RFC3339Nano))
			require.NoError(ht, err)
			runs = append(runs, r)
		}
		in := db.PruneCronRuns{ProjectID: project.ID, Actor: "operator", Before: cutoff, Keep: keep, Limit: limit, DryRun: true}
		if narrow == 1 {
			in.JobUID = fmt.Sprintf("%026d", 1)
		}
		if narrow == 2 {
			in.WorkflowUID = fmt.Sprintf("%026d", 1)
		}
		eligible := []run{}
		for _, r := range runs {
			newer := 0
			for _, other := range runs {
				if other.kind == r.kind && other.group == r.group && (other.created.After(r.created) || (other.created.Equal(r.created) && other.id > r.id)) {
					newer++
				}
			}
			if newer < keep || r.status == "running" || !r.created.Before(cutoff) || !r.updated.Before(cutoff) {
				continue
			}
			if in.JobUID != "" && (r.job == nil || *r.job != in.JobUID) {
				continue
			}
			if in.WorkflowUID != "" && (r.workflow == nil || *r.workflow != in.WorkflowUID) {
				continue
			}
			eligible = append(eligible, r)
		}
		sort.Slice(eligible, func(i, j int) bool {
			if eligible[i].created.Equal(eligible[j].created) {
				return eligible[i].id < eligible[j].id
			}
			return eligible[i].created.Before(eligible[j].created)
		})
		more := len(eligible) > limit
		if more {
			eligible = eligible[:limit]
		}
		// Aggregate the model's truncated batch independently from the store.
		wantGroups := []db.CronRunPruneGroup{}
		for _, r := range eligible {
			index := -1
			for i, group := range wantGroups {
				if (r.kind == 0 && group.JobUID != nil && *group.JobUID == r.group) || (r.kind == 1 && group.WorkflowUID != nil && *group.WorkflowUID == r.group) {
					index = i
					break
				}
			}
			if index < 0 {
				group := db.CronRunPruneGroup{Counts: map[string]int{"succeeded": 0, "failed": 0, "cancelled": 0, "unknown": 0}, OldestCreatedAt: r.created, NewestCreatedAt: r.created}
				id := r.group
				if r.kind == 0 {
					group.JobUID = &id
				} else {
					group.WorkflowUID = &id
				}
				index = len(wantGroups)
				wantGroups = append(wantGroups, group)
			}
			group := &wantGroups[index]
			group.Counts[r.status]++
			if r.created.Before(group.OldestCreatedAt) {
				group.OldestCreatedAt = r.created
			}
			if r.created.After(group.NewestCreatedAt) {
				group.NewestCreatedAt = r.created
			}
		}
		preview, err := store.PruneCronRuns(ctx, in)
		require.NoError(ht, err)
		require.Equal(ht, len(eligible), preview.Pruned)
		require.Equal(ht, more, preview.More)
		require.Zero(ht, preview.ResetAfterEventID)
		in.DryRun = false
		result, err := store.PruneCronRuns(ctx, in)
		require.NoError(ht, err)
		require.Equal(ht, wantGroups, preview.Groups)
		require.Equal(ht, wantGroups, result.Groups)
		require.Equal(ht, len(eligible), result.Pruned)
		removed := map[string]bool{}
		for _, r := range eligible {
			removed[r.id] = true
		}
		for _, r := range runs {
			_, err := store.CronRun(ctx, project.ID, r.id)
			if removed[r.id] {
				require.ErrorIs(ht, err, db.ErrNotFound)
			} else {
				require.NoError(ht, err)
			}
		}
	}, hegel.WithTestCases(1000))
	return nil
}

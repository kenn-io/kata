package tui

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronReviewNativeFillsDoNotStarveAdapters(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	adapter := j
	adapter.UID = "quiet-adapter"
	p := cronData{}
	for i := range 201 {
		native := j
		native.UID = fmt.Sprintf("native-%03d", i)
		nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&native.Definition)
		p.jobs = append(p.jobs, native)
	}
	p.jobs = append(p.jobs, adapter)
	a := &cronEnrichAPI{read: func(_ context.Context, job string) ([]generated.CronRun, error) {
		return []generated.CronRun{{UID: job + "-run", JobUID: new(job), Status: "succeeded"}}, nil
	}}
	cronEnrich(t.Context(), a, 7, &p)
	require.NoError(t, p.lastErr[adapter.UID])
	require.Equal(t, adapter.UID+"-run", p.lastRuns[adapter.UID].UID)
	require.Equal(t, "native-000-run", p.lastRuns["native-000"].UID, "native observations still feed exact display and status filters")
	require.Error(t, p.lastErr["native-200"], "native display lookups remain bounded")
	p = cronData{}
	for i := range 210 {
		job := j
		job.UID = fmt.Sprintf("ordered-%03d", i)
		job.Definition.Enabled = i >= 150
		p.jobs = append(p.jobs, job)
	}
	cronEnrich(t.Context(), a, 7, &p)
	for _, job := range p.jobs[150:] {
		require.NoError(t, p.lastErr[job.UID], "enabled jobs must be admitted before paused display reads")
		require.Equal(t, job.UID+"-run", p.lastRuns[job.UID].UID)
	}

	{
		j := cronFixture(t).cron.data.jobs[0]
		nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
		a := &cronEnrichAPI{read: func(_ context.Context, job string) ([]generated.CronRun, error) {
			return []generated.CronRun{{UID: "native-run", JobUID: new(job)}}, nil
		}}
		p := cronData{jobs: []generated.CronJob{j}}
		cronEnrich(t.Context(), a, 7, &p)
		require.Equal(t, "native-run", p.lastRuns[j.UID].UID)
		require.Equal(t, 1, a.parents)
	}
}
func TestCronReviewSharedIssueReadOnce(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
	p := cronData{jobs: []generated.CronJob{j}, runs: []generated.CronRun{{UID: "adapter-run", Status: "running", Actor: "operator", IssueUID: new(nextParentUID)}}}
	var reads atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/7/issues/"+nextParentUID {
			reads.Add(1)
			respondJSON(t, w, map[string]any{"issue": map[string]any{"status": "open", "metadata": map[string]any{"work.attention": "needs-human"}}})
		} else {
			require.Equal(t, "/api/v1/projects/7/cron/runs", r.URL.Path)
			respondJSON(t, w, map[string]any{"runs": []any{}})
		}
	})
	cronEnrich(t.Context(), newUndoClient(c), 7, &p)
	require.Equal(t, int32(1), reads.Load())
	require.True(t, p.parentOpen[nextParentUID])
	require.Equal(t, "blocked", cronRunHealth(p, p.runs[0]).state)

	{
		m := cronFixture(t)
		a := waitReadAPI(nil)
		base := &cronWaitFetchAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, waiter: a}
		r := generated.CronRun{UID: "run-example", Status: "running", Actor: "operator", IssueUID: new("issue-example"), JobUID: new(m.cron.data.jobs[0].UID)}
		p := cronData{jobs: m.cron.data.jobs, runs: []generated.CronRun{r}}
		cronEnrich(t.Context(), newUndoClient(base), 7, &p)
		require.Equal(t, "blocked", cronRunHealth(p, r).state)
		require.Equal(t, 1, a.waitCalls["issue-example"])
		cronEnrich(t.Context(), newUndoClient(&cronReadTestAPI{data: *m.cron.data}), 7, &p)
		require.Equal(t, "running", cronRunHealth(p, r).state)
		require.Equal(t, "attention unavailable", cronRunHealth(p, r).reason)
	}
}

func TestCronReviewFakeUsesCreationCursor(t *testing.T) {
	now := snapshotFixedNow
	a := &cronReadTestAPI{data: cronData{runs: []generated.CronRun{
		{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", CreatedAt: now.Add(-time.Hour), JobUID: new("job")},
		{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", CreatedAt: now, JobUID: new("job")},
		{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", CreatedAt: now, JobUID: new("job")},
		{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", CreatedAt: now.Add(time.Hour), JobUID: new("01ARZ3NDEKTSV4RRFFQ69G5FB0")},
	}}}
	page, next, err := a.ListCronJobRuns(t.Context(), 7, "job", "", 1)
	require.NoError(t, err)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAB", page[0].UID)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAB", next)
	page, next, err = a.ListCronJobRuns(t.Context(), 7, "job", next, 1)
	require.NoError(t, err)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAA", page[0].UID)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAA", next)
	page, _, err = a.ListCronJobRuns(t.Context(), 7, "job", next, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAZ", page[0].UID)
	_, _, err = a.ListCronJobRuns(t.Context(), 7, "job", "01ARZ3NDEKTSV4RRFFQ69G5FB1", 1)
	require.Error(t, err)
}

func TestCronReviewToastShortcutEnablesUnseen(t *testing.T) {
	m := cronNoticeFixture(t)
	m.cron.unseenOnly = true
	m.toast = &toast{cronFailure: true, expiresAt: snapshotFixedNow.Add(time.Minute)}
	m, _ = m.routeCronKey(keyRune('u'))
	require.True(t, m.cron.unseenOnly)
	m.toast = nil
	m, _ = m.routeCronKey(keyRune('u'))
	require.False(t, m.cron.unseenOnly, "ordinary u keeps its toggle behavior")
}

func TestCronReviewHealthCellsKeepRowBand(t *testing.T) {
	defer snapshotInit(t)()
	for _, mode := range []colorMode{colorDark, colorLight} {
		for _, cursor := range []int{0, 1} {
			m := cronHealthFixture(t)
			m.width = 120
			m.cron.lists[cronJobs].cursor = cursor
			applyColorMode(mode, mode == colorDark)
			lines := strings.Split(renderCron(m), "\n")
			row := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(stripANSI(line), "job-failing") })
			require.NotEqual(t, -1, row)
			style := altRowStyle
			if cursor == 1 {
				style = cursorRowStyle
			}
			expected := attentionTerminalCells(style.Render("x"))[0]
			cells := attentionTerminalCells(lines[row])
			require.Len(t, cells, m.width)
			for x, cell := range cells {
				require.Equal(t, expected.background, cell.background, "mode %v cursor %d column %d", mode, cursor, x)
			}
		}
	}
}

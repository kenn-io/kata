package tui

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// A successful observation before a replacement once instant does not satisfy it.
func TestCronReworkRescheduledOnce(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(snapshotFixedNow.Add(-30 * time.Minute).Format(time.RFC3339))}
	p := cronData{lastErr: map[string]error{}, lastRuns: map[string]generated.CronRun{j.UID: {Status: "succeeded", CreatedAt: snapshotFixedNow.Add(-time.Hour)}}}
	require.Equal(t, "missed", cronHealth(p, j, snapshotFixedNow).state)
}

func TestCronReworkNativeLastRunFill(t *testing.T) {
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

func TestCronReworkReplicaThroughUndo(t *testing.T) {
	a := &cronReplicaTestAPI{cronReadTestAPI: &cronReadTestAPI{data: *cronFixture(t).cron.data}, replica: cronReplica{role: "spoke"}}
	p := cronData{}
	cronFetch(t.Context(), newUndoClient(a), 7, &p)
	require.Equal(t, "spoke", p.replica.role)
}

func TestCronReworkIssueFetchByUID(t *testing.T) {
	m := cronIssueFixture(t, 80)
	var fetched atomic.Bool
	m.api = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/7/issues/issue-example" {
			fetched.Store(true)
			respondJSON(t, w, map[string]any{"issue": map[string]any{"uid": "issue-example", "project_id": 7, "short_id": "abcd", "title": "Linked issue", "status": "open"}})
			return
		}
		http.Error(w, "fixture not found", http.StatusNotFound)
	})
	m, cmd := m.cronOpenIssue("issue-example")
	require.NotNil(t, cmd)
	for _, msg := range runCmdTree(cmd) {
		if _, ok := msg.(detailFetchedMsg); ok {
			m, _ = updateModel(m, msg)
		}
	}
	require.True(t, fetched.Load(), "opening a UID link must fetch the issue, even before its short ID is known")
	require.Equal(t, "Linked issue", m.detail.issue.Title)
}

func TestCronReworkCorrectedNotice(t *testing.T) {
	for _, announced := range []bool{false, true} {
		m := cronNoticeFixture(t)
		m, _ = m.cronNoticeEvent(cronNoticeEvent("corrected", "job", snapshotFixedNow))
		if announced {
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			m.toast = nil
		}
		corrected := cronNoticeEvent("corrected", "job", snapshotFixedNow)
		corrected.cronRun.Status = "succeeded"
		m, _ = m.cronNoticeEvent(corrected)
		require.NotContains(t, m.cronNotices.pending, "corrected")
		require.NotContains(t, m.cronNotices.announced, "corrected")
		if !announced {
			m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
			require.Nil(t, m.toast)
		}
		m, cmd := m.cronNoticeEvent(cronNoticeEvent("next", "job", snapshotFixedNow))
		require.NotNil(t, cmd, "corrected failure must release job suppression")
	}
}

func TestCronReworkUnseenFillReachableWithTokens(t *testing.T) {
	m := cronSeenFixture(t)
	r := generated.CronRun{UID: "quiet-failure", JobUID: new(m.cron.data.jobs[0].UID), Status: "failed", Actor: "operator", UpdatedAt: snapshotFixedNow}
	m.cron.data.lastRuns = map[string]generated.CronRun{*r.JobUID: r}
	m = m.cronRemember([]generated.CronRun{r})
	m.cron.lists[cronRuns].query = "status:failed actor:operator"
	m = sendRune(m, 'u')
	require.Positive(t, m.cronUnseenCount())
	_, _, lines := cronScreen(m)
	found := false
	for _, line := range lines {
		if line.target.uid == r.UID {
			found = true
		}
	}
	require.True(t, found, "the unseen review must include failures counted from quiet job fills")
	m = sendKey(m, tea.KeyEnter)
	require.Equal(t, r.UID, m.cron.stack[len(m.cron.stack)-1].ref.uid)
}

type cronReworkRunAPI struct {
	*cronReadTestAPI
	run generated.CronRun
	err error
}

func (a *cronReworkRunAPI) readCronRun(ctx context.Context, _ int64, uid string) (generated.CronRun, error) {
	if err := ctx.Err(); err != nil {
		return generated.CronRun{}, err
	}
	if uid != a.run.UID {
		return generated.CronRun{}, errors.New("wrong run UID")
	}
	return a.run, a.err
}
func TestCronReworkOldRunRefresh(t *testing.T) {
	m := cronFixture(t)
	old := generated.CronRun{UID: "old-run", Revision: 1, Status: "running", IssueUID: new("old-issue")}
	m.cron.data.lookup = map[string]generated.CronRun{old.UID: old}
	m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, old.UID}}}
	fresh := old
	fresh.Revision = 2
	fresh.Status = "succeeded"
	a := &cronReworkRunAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, run: fresh}
	m.api = newUndoClient(a)
	m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
	got, ok := m.cron.data.run(old.UID)
	require.True(t, ok)
	require.Equal(t, "succeeded", got.Status)
	a.err = errors.New("run refresh offline")
	m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
	_, _, lines := cronScreen(m)
	text := ""
	for _, line := range lines {
		text += line.text + "\n"
	}
	require.Contains(t, text, "run refresh offline")
}

func TestCronReworkJobHasOneHealthRow(t *testing.T) {
	m := cronFixture(t)
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, m.cron.data.jobs[0].UID}}}
	_, _, lines := cronScreen(m)
	require.NotContains(t, cronLinesText(lines), "State:")
	require.Contains(t, cronLinesText(lines), "Health:")
}

func TestCronReworkCombinedBlockedFilter(t *testing.T) {
	m := cronSeenFixture(t)
	j := m.cron.data.jobs[0]
	j.Definition.Trigger = generated.TriggerResponse{Kind: "manual"}
	r := generated.CronRun{UID: "waiting-run", JobUID: new(j.UID), IssueUID: new("waiting-issue"), Status: "running", Actor: "operator", CreatedAt: snapshotFixedNow}
	m.cron.data.jobs = []generated.CronJob{j}
	m.cron.data.runs = []generated.CronRun{r}
	m.cron.runPage.runs = []generated.CronRun{r}
	m.cron.data.lastRuns = map[string]generated.CronRun{j.UID: r}
	m.cron.data.lastErr = map[string]error{}
	m.cron.data.waits = map[string]cronRunWait{"waiting-issue": {open: true, metadata: map[string]any{"work.attention": "needs-human"}}}
	m.cron.lists[cronJobs].query = "health:blocked status:running"
	_, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0].text, "blocked")
	m.cron.tab = cronRuns
	m.cron.lists[cronRuns].query = "status:running actor:operator"
	_, _, lines = cronScreen(m)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0].text, "blocked")
}

func TestCronReworkPlainRunDetailDoesNotRepeatStatus(t *testing.T) {
	m := cronFixture(t)
	r := m.cron.data.runs[0]
	r.Status = "running"
	r.IssueUID = new("issue")
	m.cron.data.runs = []generated.CronRun{r}
	m.cron.data.waits = map[string]cronRunWait{"issue": {open: true}}
	m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, r.UID}}}
	_, _, lines := cronScreen(m)
	require.NotContains(t, cronLinesText(lines), "Health:")
	require.NotContains(t, cronLinesText(lines), "executor recorded")
}

func TestCronReworkRunClientByUID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/7/cron/runs/run-example", r.URL.Path)
		respondJSON(t, w, map[string]any{"run": map[string]any{"uid": "run-example", "status": "failed", "revision": 2}})
	})
	reader, ok := any(newUndoClient(c)).(cronRunAPI)
	require.True(t, ok)
	r, err := reader.readCronRun(t.Context(), 7, "run-example")
	require.NoError(t, err)
	require.Equal(t, "run-example", r.UID)
	require.Equal(t, "failed", r.Status)
}

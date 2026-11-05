package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
)

func cronBlockedFixture(t *testing.T) Model {
	m := cronFixture(t)
	r := m.cron.data.runs[0]
	r.Status = "running"
	r.IssueUID = new("issue-example")
	r.EndedAt = nil
	m.cron.data.runs = []generated.CronRun{r}
	m.cron.runPage.runs = []generated.CronRun{r}
	m.cron.data.lastRuns = map[string]generated.CronRun{deref(r.JobUID): r}
	m.cron.data.waits = map[string]cronParent{"issue-example": {open: true, metadata: map[string]any{"work.attention": "needs-human"}}}
	return m
}
func cronScreenText(m Model) string {
	_, _, lines := cronScreen(m)
	var s strings.Builder
	for _, l := range lines {
		s.WriteString(ansi.Strip(l.text))
		s.WriteByte('\n')
	}
	return s.String()
}

// Contract: inference decorates running without changing its stored status;
// only primary blocked job health is included in the roll-up.
func TestCronWaitRendering(t *testing.T) {
	m := cronBlockedFixture(t)
	m.width = 140
	title, _, _ := cronScreen(m)
	require.Contains(t, title, "1 blocked")
	require.Contains(t, cronScreenText(m), "blocked")
	m.cron.tab = cronRuns
	require.Contains(t, cronScreenText(m), "running · blocked")
	require.Equal(t, "running", m.cron.data.runs[0].Status)
	r := m.cron.data.runs[0]
	m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, r.UID}}}
	text := cronScreenText(m)
	require.Contains(t, text, "Health:     blocked")
	require.Contains(t, text, "Needs human attention")
	m.cron.data.waits = nil
	require.Contains(t, cronScreenText(m), "attention unavailable")
	m.cron.data.waits = map[string]cronParent{"issue-example": {open: true, metadata: map[string]any{"work.attention": "stuck"}}}
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, deref(r.JobUID)}}}
	require.Contains(t, cronScreenText(m), "running · blocked")
	m.cron.data.jobs[0].Definition.TimeoutSeconds = new(int64(1))
	m.cron.stack = nil
	m.cron.tab = cronJobs
	title, _, _ = cronScreen(m)
	require.Contains(t, title, "1 overdue")
	require.NotContains(t, title, "1 blocked")

	{
		defer snapshotInit(t)()
		m := cronBlockedFixture(t)
		assertGolden(t, "cron-jobs-blocked", m.viewBody())
		m.cron.tab = cronRuns
		m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, m.cron.data.runs[0].UID}}}
		assertGolden(t, "cron-run-detail-blocked", m.viewBody())
	}
}

func TestCronWaitMetadataEventsRefreshAllLoadedRuns(t *testing.T) {
	m := cronBlockedFixture(t)
	require.True(t, m.cronParentEvent(eventReceivedMsg{eventType: "issue.metadata_updated", issueUID: "issue-example"}))
	m.cron.data.runs = nil
	m.cron.data.lastRuns = nil
	m.cron.data.lookup = map[string]generated.CronRun{"historical": {UID: "historical", Status: "running", IssueUID: new("history-issue")}}
	require.True(t, m.cronParentEvent(eventReceivedMsg{eventType: "issue.metadata_updated", issueUID: "history-issue"}))
	m.cron.data.lookup["historical"] = generated.CronRun{UID: "historical", Status: "failed", IssueUID: new("history-issue")}
	require.False(t, m.cronParentEvent(eventReceivedMsg{eventType: "issue.metadata_updated", issueUID: "history-issue"}))
	require.False(t, m.cronParentEvent(eventReceivedMsg{eventType: "issue.metadata_updated", issueUID: "other-issue"}))
}

func TestCronWaitNativeRunDoesNotAddAttentionRead(t *testing.T) {
	m := cronBlockedFixture(t)
	j := m.cron.data.jobs[0]
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(time.Hour / time.Second))})(&j.Definition)
	m.cron.data.jobs = []generated.CronJob{j}
	a := waitReadAPI(nil)
	cronEnrich(t.Context(), a, 7, m.cron.data)
	require.Equal(t, map[string]int{nextParentUID: 1}, a.waitCalls, "only the shared native parent read is needed")
}

func TestCronWaitPlainRunDetailDoesNotRepeatStatus(t *testing.T) {
	m := cronFixture(t)
	r := m.cron.data.runs[0]
	r.Status = "running"
	r.IssueUID = new("issue")
	m.cron.data.runs = []generated.CronRun{r}
	m.cron.data.waits = map[string]cronParent{"issue": {open: true}}
	m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, r.UID}}}
	_, _, lines := cronScreen(m)
	require.Equal(t, 1, strings.Count(cronLinesText(lines), "running"), "plain status must appear exactly once")
	require.Contains(t, cronLinesText(lines), "Status:     running")
	require.NotContains(t, cronLinesText(lines), "Health:")
}

// A running run's issue that moved to another project still decides Cron
// health: its events carry the destination project but refresh Cron at once,
// and the refreshed attention read, not the stale snapshot, sets the health.
func TestCronMovedRunIssueEventRefreshesCron(t *testing.T) {
	moved := func(uid string) eventReceivedMsg {
		return eventReceivedMsg{eventType: "issue.metadata_updated", projectID: 8, issueUID: uid}
	}
	fixture := func(t *testing.T, wait func(context.Context, string) (cronParent, error)) (Model, *cronWaitReadAPI) {
		m := cronBlockedFixture(t)
		m.width = 140
		require.Equal(t, viewCron, m.view)
		require.Equal(t, m.cron.events, m.cron.data.events)
		require.False(t, m.cron.out)
		waiter := waitReadAPI(nil)
		waiter.wait = wait
		m.api = &cronWaitFetchAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, waiter: waiter}
		title, _, _ := cronScreen(m)
		require.Contains(t, title, "1 blocked")
		return m, waiter
	}
	cleared := func(context.Context, string) (cronParent, error) {
		return cronParent{open: true, metadata: map[string]any{}}, nil
	}
	for _, tc := range []struct {
		name    string
		wait    func(context.Context, string) (cronParent, error)
		blocked bool
		reason  string
	}{
		{"attention cleared", cleared, false, ""},
		{"notification still pending", func(context.Context, string) (cronParent, error) {
			return cronParent{open: true, metadata: map[string]any{notification.MetadataKey("worker"): map[string]any{"from": "operator", "message": "review"}}}, nil
		}, true, "waiting on worker: review"},
		{"destination unreadable", func(context.Context, string) (cronParent, error) { return cronParent{}, errors.New("forbidden") }, false, "attention unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, waiter := fixture(t, tc.wait)
			m = cronUpdateTest(m, moved("issue-example"))
			require.True(t, m.cron.out, "the moved issue's event refreshes Cron")
			require.False(t, m.pendingRefetch, "the selected project's issue list is not invalidated")
			m = cronLoadTest(t, m)
			require.Equal(t, 1, waiter.waitCalls["issue-example"])
			title, _, _ := cronScreen(m)
			require.Equal(t, tc.blocked, strings.Contains(title, "1 blocked"), title)
			m.cron.tab = cronRuns
			m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, m.cron.data.runs[0].UID}}}
			text := cronScreenText(m)
			require.Equal(t, tc.blocked, strings.Contains(text, "Health:     blocked"), text)
			if tc.reason != "" {
				require.Contains(t, text, tc.reason)
			}
		})
	}
	t.Run("retained history only", func(t *testing.T) {
		m, _ := fixture(t, cleared)
		r := m.cron.data.runs[0]
		r.UID, r.IssueUID = "retained-run", new("retained-issue")
		m.cron.runPage.runs = append(m.cron.runPage.runs, r)
		m = m.cronRemember([]generated.CronRun{r})
		m = cronUpdateTest(m, moved("retained-issue"))
		require.True(t, m.cron.out)
	})
	t.Run("unrelated or terminal issue", func(t *testing.T) {
		m, _ := fixture(t, cleared)
		r := m.cron.data.runs[0]
		r.UID, r.Status, r.IssueUID = "ended-run", "succeeded", new("ended-issue")
		m = m.cronRemember([]generated.CronRun{r})
		for _, uid := range []string{"other-issue", "ended-issue"} {
			m = cronUpdateTest(m, moved(uid))
			require.False(t, m.cron.out, uid)
			require.Equal(t, m.cron.data.events, m.cron.events, uid)
		}
	})
	t.Run("during an outstanding fetch", func(t *testing.T) {
		m, waiter := fixture(t, cleared)
		m = cronUpdateTest(m, moved("issue-example"))
		first := m.fetchCron()
		m = cronUpdateTest(m, moved("issue-example"))
		require.True(t, m.cron.out)
		m = cronUpdateTest(m, first())
		require.True(t, m.cron.out, "the later event is read by a follow-up refresh")
		m = cronLoadTest(t, m)
		require.Equal(t, 2, waiter.waitCalls["issue-example"])
		require.Equal(t, m.cron.events, m.cron.data.events)
	})
}

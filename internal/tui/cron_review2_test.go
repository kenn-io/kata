package tui

import (
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// Contract: refreshing an inspected run refreshes its status everywhere the
// already-loaded record is visible; retained pages preserve history, not stale revisions.
func TestIndependentReview2RetainedRunRevision(t *testing.T) {
	for _, jobHistory := range []bool{false, true} {
		t.Run(map[bool]string{false: "project", true: "job"}[jobHistory], func(t *testing.T) {
			m := cronSeenFixture(t)
			old := m.cron.data.runs[0]
			old.UID, old.Status, old.Revision = "old-running", "running", 1
			old.CreatedAt = snapshotFixedNow.Add(-24 * time.Hour)
			old.EndedAt = nil
			old.IssueUID = new("old-issue")
			head := old
			head.UID, head.Status, head.Revision = "current-head", "succeeded", 1
			head.CreatedAt = snapshotFixedNow
			m.cron.data.runs = []generated.CronRun{head}
			m.cron.data.lastRuns = map[string]generated.CronRun{*head.JobUID: head}
			m.cron.data.lookup = map[string]generated.CronRun{old.UID: old}
			m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{head, old}, ready: true, older: true}
			m.cron.jobPage = cronHistoryPage{job: *old.JobUID, runs: []generated.CronRun{head, old}, ready: true, older: true}
			m.cron.tab = cronRuns
			m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, old.UID}}}
			if jobHistory {
				m.cron.stack = append([]cronFrame{{ref: cronRef{cronJobs, *old.JobUID}}}, m.cron.stack...)
			}
			fresh := old
			fresh.Status, fresh.Revision = "succeeded", 2
			fresh.EndedAt = new(snapshotFixedNow)
			m.api = &cronReworkRunAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, run: fresh}
			m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
			got, ok := m.cron.data.run(old.UID)
			require.True(t, ok)
			require.Equal(t, "succeeded", got.Status, "detail refresh succeeds")
			m.cron.stack = m.cron.stack[:len(m.cron.stack)-1]
			_, _, lines := cronScreen(m)
			i := slices.IndexFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID })
			require.NotEqual(t, -1, i)
			require.Contains(t, lines[i].text, "succeeded", "back to retained history must agree with refreshed detail")
			m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
			_, _, lines = cronScreen(m)
			i = slices.IndexFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID })
			require.NotEqual(t, -1, i)
			require.Contains(t, lines[i].text, "succeeded", "later events must not downgrade a retained page to its old revision")
		})
	}
}

// Contract: the unseen marker and count describe the same highest loaded revision.
func TestIndependentReview2RetainedFailureMarker(t *testing.T) {
	m := cronSeenFixture(t)
	old := m.cron.data.runs[0]
	old.UID, old.Status, old.Revision = "corrected-failure", "failed", 1
	old.EndedAt = new(snapshotFixedNow)
	fresh := old
	fresh.Status, fresh.Revision = "succeeded", 2
	m.cron.data.runs = nil
	m.cron.data.lastRuns = nil
	m.cron.data.lookup = map[string]generated.CronRun{old.UID: fresh}
	m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{old}, ready: true, older: true}
	m.cron.tab = cronRuns
	require.Zero(t, m.cronUnseenCount(), "newest loaded revision is successful")
	_, _, lines := cronScreen(m)
	require.Len(t, lines, 1)
	require.NotContains(t, lines[0].text, "•", "zero unseen count must not show an unseen failure marker")
}

// Contract: every retained page row remains a working detail link after an
// event refresh, and reviewing a failure must display the record being acknowledged.
func TestIndependentReview2RetainedRunLink(t *testing.T) {
	m := cronSeenFixture(t)
	head := m.cron.data.runs[0]
	old := head
	old.UID, old.Status = "retained-failure", "failed"
	old.CreatedAt = snapshotFixedNow.Add(-24 * time.Hour)
	old.EndedAt = new(old.CreatedAt)
	m.cron.data.runs = []generated.CronRun{head}
	m.cron.data.lastRuns = map[string]generated.CronRun{*head.JobUID: head}
	m.cron.data.lookup = map[string]generated.CronRun{old.UID: old}
	m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{head, old}, ready: true, older: true}
	m.cron.tab = cronRuns
	m.api = &cronReadTestAPI{data: *m.cron.data}
	m.cron.events++
	m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
	_, _, lines := cronScreen(m)
	i := slices.IndexFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID })
	require.NotEqual(t, -1, i, "event retains the older row")
	m.cron.lists[cronRuns].cursor = i
	m, _ = m.routeCronKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Zero(t, m.cronUnseenCount(), "opening the retained row acknowledges it")
	_, _, detail := cronScreen(m)
	require.NotContains(t, cronLinesText(detail), "no longer in the loaded data", "an acknowledged run must still be readable")
	require.Contains(t, cronLinesText(detail), "failed")
}

// Contract: unseen-only is a filter over the newest-first Runs listing, with
// quiet fills added; it should not reverse the order of already visible failures.
func TestIndependentReview2UnseenOrder(t *testing.T) {
	m := cronSeenFixture(t)
	old := m.cron.data.runs[0]
	old.UID, old.Status = "01ARZ3NDEKTSV4RRFFQ69G5FAA", "failed"
	old.CreatedAt = snapshotFixedNow.Add(-time.Hour)
	old.EndedAt = new(old.CreatedAt)
	newest := old
	newest.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAB"
	newest.CreatedAt = snapshotFixedNow
	newest.EndedAt = new(newest.CreatedAt)
	rows := []generated.CronRun{newest, old}
	m.cron.tab = cronRuns
	m.cron.data.runs = rows
	m.cron.data.lastRuns = nil
	m.cron.data.lookup = nil
	m.cron.runPage = cronHistoryPage{runs: rows, ready: true}
	_, _, before := cronScreen(m)
	m.cron.unseenOnly = true
	_, _, after := cronScreen(m)
	require.Equal(t, before[0].target, after[0].target, "enabling a filter must retain the newest-first run order")
}

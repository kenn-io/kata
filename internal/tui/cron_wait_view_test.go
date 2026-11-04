package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
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
	m.cron.data.waits = map[string]cronRunWait{"issue-example": {open: true, metadata: map[string]any{"work.attention": "needs-human"}}}
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
	m.cron.data.waits = map[string]cronRunWait{"issue-example": {open: true, metadata: map[string]any{"work.attention": "stuck"}}}
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, deref(r.JobUID)}}}
	require.Contains(t, cronScreenText(m), "running · blocked")
	m.cron.data.jobs[0].Definition.TimeoutSeconds = new(int64(1))
	m.cron.stack = nil
	m.cron.tab = cronJobs
	title, _, _ = cronScreen(m)
	require.Contains(t, title, "1 overdue")
	require.NotContains(t, title, "1 blocked")
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

func TestSnapshot_CronBlocked(t *testing.T) {
	defer snapshotInit(t)()
	m := cronBlockedFixture(t)
	assertGolden(t, "cron-jobs-blocked", m.viewBody())
	m.cron.tab = cronRuns
	m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, m.cron.data.runs[0].UID}}}
	assertGolden(t, "cron-run-detail-blocked", m.viewBody())
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

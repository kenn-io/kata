package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// A reset can omit the correcting event: current loaded success must release
// the old job failure's notice suppression just as a correcting SSE event does.
func TestIndependentReview3CorrectionAfterReset(t *testing.T) {
	m := cronNoticeFixture(t)
	job := m.cron.data.jobs[0].UID
	event := cronNoticeEvent("corrected-run", job, snapshotFixedNow)
	m, _ = m.cronNoticeEvent(event)
	m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
	require.NotNil(t, m.toast)
	m.toast = nil
	current := m.cron.data.runs[0]
	current.UID, current.Status, current.Revision = "corrected-run", "succeeded", 2
	data := *m.cron.data
	data.runs = []generated.CronRun{current}
	data.lastRuns = map[string]generated.CronRun{job: current}
	data.lookup = map[string]generated.CronRun{current.UID: current}
	data.resetPages = true
	m = m.cronLoaded(cronLoadedMsg(data))
	require.Zero(t, m.cronUnseenCount())
	m, cmd := m.cronNoticeEvent(cronNoticeEvent("new-failure", job, snapshotFixedNow))
	require.NotNil(t, cmd, "a corrected failure is no longer an unseen failure of this job")
	m, _ = m.cronNoticeFlush(cronNoticeTickFor(m))
	require.NotNil(t, m.toast)
}

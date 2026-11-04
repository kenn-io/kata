package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronReview3FanoutAPI struct {
	*cronReadTestAPI
	fills atomic.Int64
}

func (a *cronReview3FanoutAPI) ListCronJobRuns(ctx context.Context, id int64, job, before string, limit int64) ([]generated.CronRun, string, error) {
	if job == "" {
		return a.cronReadTestAPI.ListCronJobRuns(ctx, id, job, before, limit)
	}
	a.fills.Add(1)
	return nil, "", nil // These enabled jobs have never run.
}

// Separately completed fast reads must not bypass the outstanding event debounce
// and repeatedly sweep every quiet job before that debounce has fired.
func TestCronReview3EventRefreshFanout(t *testing.T) {
	m := cronSeenFixture(t)
	data := *m.cron.data
	for i := range 2 {
		j := data.jobs[0]
		j.UID = fmt.Sprintf("quiet-job-%d", i)
		data.jobs = append(data.jobs, j)
	}
	a := &cronReview3FanoutAPI{cronReadTestAPI: &cronReadTestAPI{data: data}}
	m.api = a
	defer func() { m.cron.reads.stop() }()
	for range 3 {
		// This is Update's event handler followed by its Cron scheduling hook.
		// Leave the debounce command pending while completing any immediate fetch.
		next, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: 7})
		m = next.(Model)
		nextModel, cmd := m.cronSync()
		m = nextModel
		if cmd != nil {
			m = m.cronLoaded(cmd().(cronLoadedMsg))
		}
	}
	t.Logf("quiet-job fills before the event debounce: %d", a.fills.Load())
	require.LessOrEqual(t, a.fills.Load(), int64(2), "one pending event debounce must coalesce repeated whole-project enrichment")
}

// A failed persisted read cannot establish that a loaded failure was never seen.
func TestCronReview3CorruptSeenCount(t *testing.T) {
	m := cronSeenFixture(t)
	r := m.cron.data.runs[0]
	r.Status = "failed"
	m.cron.data.runs = []generated.CronRun{r}
	path := filepath.Join(t.TempDir(), "cron-seen.json")
	require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0600))
	m.cronSeen = cronSeenState{path: path}
	msg := m.cronSeenCommand(false)().(cronSeenResultMsg)
	require.Error(t, msg.err)
	m, _ = m.cronSeenResult(msg)
	screen := stripANSI(renderCron(m))
	require.Contains(t, screen, "seen state unavailable")
	require.NotContains(t, screen, "1 unseen failed", "unreadable persisted acknowledgements leave the count unknown")
}

// A failed jobs read may retain definitions, but cannot replace fresh head
// evidence with an older cached last run in the visible Last run column.
func TestCronReview3JobsErrorKeepsFreshLastRun(t *testing.T) {
	m := cronSeenFixture(t)
	job := m.cron.data.jobs[0]
	fresh := m.cron.data.runs[0]
	fresh.UID, fresh.Status = "new-head-run", "failed"
	fresh.CreatedAt = fresh.CreatedAt.Add(time.Hour)
	data := *m.cron.data
	data.jobsErr = errors.New("jobs unavailable")
	data.runs = []generated.CronRun{fresh}
	m.api = &cronReadTestAPI{data: data}
	msg := m.fetchCron()().(cronLoadedMsg)
	require.Equal(t, fresh.UID, msg.lastRuns[job.UID].UID, "the successful head read supplies fresh last-run evidence")
	m = m.cronLoaded(msg)
	require.Equal(t, "unavailable", cronHealth(*m.cron.data, job, snapshotFixedNow).state)
	require.Contains(t, cronJobLast(*m.cron.data, job.UID), "failed", "Last run must retain the successful head read despite the unrelated jobs failure")
}

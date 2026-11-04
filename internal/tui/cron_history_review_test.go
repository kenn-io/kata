package tui

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

// Busy projects must keep the history a person is reading across event refreshes.
// Only the explicit refresh shortcut replaces loaded pages; errors retain them.
func TestCronReviewHistoryRefreshIntent(t *testing.T) {
	for _, kind := range []string{"event", "manual", "failed event"} {
		t.Run(kind, func(t *testing.T) {
			m := cronSeenFixture(t)
			head := m.cron.data.runs[0]
			old := head
			old.UID = "old-history"
			old.CreatedAt = head.CreatedAt.Add(-time.Hour)
			old.Status = "failed"
			m.cron.data.runs = []generated.CronRun{old}
			m.cron.data.lastRuns = map[string]generated.CronRun{*old.JobUID: old}
			m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{old}, next: old.UID, ready: true, older: true}
			m.cron.jobPage = cronHistoryPage{job: *old.JobUID, runs: []generated.CronRun{old}, next: old.UID, ready: true, older: true}
			m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, *old.JobUID}}}
			_, _, lines := cronScreen(m)
			m.cron.stack[0].cursor = slices.IndexFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID })
			require.GreaterOrEqual(t, m.cron.stack[0].cursor, 0)
			fresh := *m.cron.data
			fresh.runs = []generated.CronRun{head}
			if kind == "failed event" {
				fresh.runsErr = errors.New("offline")
			}
			m.api = &cronReadTestAPI{data: fresh}
			if kind == "manual" {
				m, _ = m.routeCronKey(keyRune('r'))
			} else {
				model, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: 7})
				m = model.(Model)
			}
			next, fetch := m.cronSync()
			m = next
			require.NotNil(t, fetch)
			m = m.cronLoaded(fetch().(cronLoadedMsg))
			m, page := m.cronSync()
			if page != nil {
				msg, ok := page().(cronPageMsg)
				require.True(t, ok)
				m = m.cronPageLoaded(msg)
			}
			hasOld := func(rows []generated.CronRun) bool {
				return slices.ContainsFunc(rows, func(r generated.CronRun) bool { return r.UID == old.UID })
			}
			if kind == "manual" {
				require.False(t, hasOld(m.cron.runPage.runs))
				require.False(t, hasOld(m.cron.jobPage.runs))
				return
			}
			require.True(t, hasOld(m.cron.runPage.runs), "event must retain project history")
			require.True(t, hasOld(m.cron.jobPage.runs), "event must retain job history")
			require.Equal(t, old.UID, m.cron.runPage.next)
			require.Equal(t, old.UID, m.cron.jobPage.next)
			_, _, lines = cronScreen(m)
			require.Equal(t, old.UID, lines[m.cron.stack[0].cursor].target.uid, "event keeps the selected run even when the head grows")
			if kind == "failed event" {
				require.ErrorContains(t, m.cron.runPage.err, "offline")
				require.ErrorContains(t, m.cron.jobPage.err, "offline")
			}
		})
	}
}

func TestCronReviewEventKeepsInflightOlderPage(t *testing.T) {
	for _, jobPage := range []bool{false, true} {
		for _, olderFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("job=%v/older-first=%v", jobPage, olderFirst), func(t *testing.T) {
				m := cronFixture(t)
				m.cron.tab = cronRuns
				old := m.cron.data.runs[0]
				old.UID = "loaded-old"
				old.CreatedAt = snapshotFixedNow.Add(-time.Hour)
				oldest := old
				oldest.UID = "requested-older"
				oldest.CreatedAt = old.CreatedAt.Add(-time.Hour)
				fresh := *m.cron.data
				fresh.runs = []generated.CronRun{old, oldest}
				for i := range 150 {
					r := old
					r.UID = fmt.Sprintf("head-%03d", i)
					r.CreatedAt = snapshotFixedNow.Add(time.Duration(i) * time.Minute)
					fresh.runs = append(fresh.runs, r)
				}
				m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{old}, next: old.UID, ready: true}
				job := ""
				if jobPage {
					job = *old.JobUID
					m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, job}}}
					m.cron.jobPage = cronHistoryPage{job: job, runs: []generated.CronRun{old}, next: old.UID, ready: true}
				}
				m.api = &cronReadTestAPI{data: fresh}
				m, request := m.cronRequestPage(job, false)
				late := request().(cronPageMsg)
				model, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: 7})
				m = model.(Model)
				m, fetch := m.cronSync()
				require.NotNil(t, fetch)
				if olderFirst {
					m = m.cronPageLoaded(late)
				}
				headMsg := fetch().(cronLoadedMsg)
				require.Len(t, headMsg.runs, 100)
				require.False(t, slices.ContainsFunc(headMsg.runs, func(r generated.CronRun) bool { return r.UID == oldest.UID }))
				m = m.cronLoaded(headMsg)
				if !olderFirst {
					m = m.cronPageLoaded(late)
				}
				m, head := m.cronSync()
				if head != nil {
					msg, ok := head().(cronPageMsg)
					require.True(t, ok)
					m = m.cronPageLoaded(msg)
				}
				page := m.cron.runPage
				if jobPage {
					page = m.cron.jobPage
				}
				require.True(t, slices.ContainsFunc(page.runs, func(r generated.CronRun) bool { return r.UID == oldest.UID }), "event must accept the valid older response in either arrival order")
				require.False(t, page.out)
				require.Empty(t, page.next, "the accepted terminal older page owns continuation")
			})
		}
	}
}

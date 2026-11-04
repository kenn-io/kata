package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

type cronPagingAPI struct {
	*cronReadTestAPI
	job, before string
	limit       int64
	page        []generated.CronRun
	next        string
	err         error
}

func (a *cronPagingAPI) ListCronJobRuns(_ context.Context, _ int64, job, before string, limit int64) ([]generated.CronRun, string, error) {
	a.job, a.before, a.limit = job, before, limit
	return slices.Clone(a.page), a.next, a.err
}

// Contract: job details have independent server-filtered history; older project
// pages never enlarge the workflow's documented newest-100 history.
func TestCronHistoryJobPagesAndLookup(t *testing.T) {
	m := cronFixture(t)
	j := m.cron.data.jobs[0]
	old := m.cron.data.runs[0]
	old.UID = "old-run"
	old.Revision = 1
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: []generated.CronRun{old}, next: "older"}
	m.api = a
	m, cmd := m.cronRequestPage(j.UID, true)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Equal(t, j.UID, a.job)
	require.Equal(t, int64(20), a.limit)
	require.Equal(t, "", a.before)
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, j.UID}}}
	_, _, lines := cronScreen(m)
	require.True(t, slices.ContainsFunc(lines, func(l cronLine) bool { return l.target == cronRef{cronRuns, old.UID} }))
	require.True(t, slices.ContainsFunc(lines, func(l cronLine) bool { return l.target.kind == cronOlder }))
	older := old
	older.UID = "older-run"
	a.page = []generated.CronRun{older}
	a.next = ""
	m, cmd = m.cronRequestPage(j.UID, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Equal(t, "older", a.before)
	require.Len(t, m.cron.jobPage.runs, 2)
	m.cron.stack = append(m.cron.stack, cronFrame{ref: cronRef{cronRuns, old.UID}})
	require.Contains(t, stripANSI(m.viewBody()), old.Status)
	// Refresh prunes old pages but must retain a run currently being inspected.
	data := *m.cron.data
	data.runs = nil
	m = m.cronLoaded(cronLoadedMsg(data))
	require.Contains(t, stripANSI(m.viewBody()), old.Status)
	m.cron.stack = []cronFrame{{ref: cronRef{cronWorkflows, m.cron.data.workflows[0].UID}}}
	_, _, lines = cronScreen(m)
	require.False(t, slices.ContainsFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID }))
}

func TestCronHistoryFencesAndSingleFlight(t *testing.T) {
	m := cronFixture(t)
	j := m.cron.data.jobs[0].UID
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: m.cron.data.runs, next: "next"}
	m.api = a
	m, cmd := m.cronRequestPage(j, true)
	stale := cmd().(cronPageMsg)
	_, second := m.cronRequestPage(j, false)
	require.Nil(t, second, "one outstanding read per history")
	// A -> B -> A reopening has the same scope, job and cursor, but a new generation.
	m, _ = m.cronRequestPage("different", true)
	m, current := m.cronRequestPage(j, true)
	m = m.cronPageLoaded(stale)
	require.True(t, m.cron.jobPage.out)
	fresh := current().(cronPageMsg)
	for _, edit := range []func(*cronPageMsg){
		func(p *cronPageMsg) { p.conn++ }, func(p *cronPageMsg) { p.scope++ },
		func(p *cronPageMsg) { p.cursor = "wrong" }, func(p *cronPageMsg) { p.job = "wrong" },
		func(p *cronPageMsg) { p.events++ },
	} {
		bad := fresh
		edit(&bad)
		next := m.cronPageLoaded(bad)
		require.True(t, next.cron.jobPage.out)
	}
	m = m.cronPageLoaded(fresh)
	require.False(t, m.cron.jobPage.out)
	require.Equal(t, "next", m.cron.jobPage.next)
	for _, event := range []tea.Msg{keyRune('r'), resetRequiredMsg{}, eventReceivedMsg{eventType: "cron.run.observed", projectID: 7}} {
		base := m
		base, cmd = base.cronRequestPage(j, false)
		late := cmd().(cronPageMsg)
		base, _ = updateModel(base, event)
		base = base.cronPageLoaded(late)
		require.NotEqual(t, late.gen, base.cron.jobPage.gen, "refresh/event/reset invalidates pages immediately")
	}
}

func TestCronHistoryFailureRetryAndForeignJob(t *testing.T) {
	m := cronFixture(t)
	j := m.cron.data.jobs[0].UID
	r := m.cron.data.runs[0]
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: []generated.CronRun{r}, next: "next"}
	m.api = a
	m, cmd := m.cronRequestPage(j, true)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	a.err = errors.New("offline")
	m, cmd = m.cronRequestPage(j, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Len(t, m.cron.jobPage.runs, 1)
	require.Equal(t, "next", m.cron.jobPage.next)
	require.ErrorContains(t, m.cron.jobPage.err, "offline")
	a.err = nil
	a.page = []generated.CronRun{{UID: "foreign", JobUID: new("other")}}
	m, cmd = m.cronRequestPage(j, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.ErrorContains(t, m.cron.jobPage.err, "job")
	require.Equal(t, "next", m.cron.jobPage.next)
	require.Len(t, m.cron.jobPage.runs, 1)
	a.page = nil
	a.next = ""
	m, cmd = m.cronRequestPage(j, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.NoError(t, m.cron.jobPage.err)
	require.Empty(t, m.cron.jobPage.next, "empty terminal page is ordinary exhaustion")
}

func TestCronRunsLoadOlderByKey(t *testing.T) {
	m := sendRune(cronFixture(t), '3')
	m.cron.data.nextBefore = "next"
	m.cron.runPage = cronHistoryPage{runs: m.cron.data.runs, next: "next", ready: true}
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: []generated.CronRun{{UID: "old", Status: "failed"}}}
	m.api = a
	_, _, lines := cronScreen(m)
	m.cron.lists[cronRuns].cursor = len(lines) - 1
	m, cmd := m.routeCronKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Empty(t, a.job)
	require.Equal(t, "next", a.before)
	require.Equal(t, int64(100), a.limit)
	title, _, _ := cronScreen(m)
	require.Contains(t, title, "2 loaded runs")
	require.NotContains(t, title, "newest 100")
}

func TestCronHistoryRetainsVisibleLookupAfterFailedRefresh(t *testing.T) {
	m := cronFixture(t)
	r := m.cron.data.runs[0]
	r.UID = "old-page"
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: []generated.CronRun{r}}
	m.api = a
	m.cron.runPage.next = "cursor"
	m, cmd := m.cronRequestPage("", false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	data := *m.cron.data
	data.runsErr = errors.New("offline")
	m = m.cronLoaded(cronLoadedMsg(data))
	m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, r.UID}}}
	_, _, lines := cronScreen(m)
	require.Contains(t, cronLinesText(lines), "Report ready")
}

func TestSnapshot_CronHistory(t *testing.T) {
	defer snapshotInit(t)()
	m := cronFixture(t)
	m.cron.runPage.next = "next"
	m = sendRune(m, '3')
	assertGolden(t, "cron-runs-paging", m.viewBody())
	m.height = 58
	j := m.cron.data.jobs[0].UID
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, j}}}
	m.cron.jobPage = cronHistoryPage{job: j, runs: m.cron.data.runs, next: "next", ready: true}
	assertGolden(t, "cron-job-history", m.viewBody())
}

// Review reproduction: A -> run -> workflow -> B -> back to A must start A's
// history again; a single cached page cannot leave the visible job loading.
func TestCronHistoryBackToJobRestartsHistory(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		m := cronFixture(t)
		job := m.cron.data.jobs[0].UID
		other := fmt.Sprintf("other-%d", hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0))))

		second := m.cron.data.jobs[0]
		second.UID = other
		m.cron.data.jobs = append(m.cron.data.jobs, second)
		a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}}
		m.api = a
		follow := func(target cronRef) {
			_, _, lines := cronScreen(m)
			i := slices.IndexFunc(lines, func(l cronLine) bool { return l.target == target })
			if i < 0 {
				ht.Fatalf("missing reachable link %+v", target)
			}
			*m.cronCursor() = i
			next, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			m = drainInboxCmds(next, cmd)
		}
		follow(cronRef{cronJobs, job})
		follow(cronRef{cronWorkflows, m.cron.data.workflows[0].UID})
		follow(cronRef{cronJobs, other})
		for range 2 {
			next, cmd := updateModel(m, keyRune('h'))
			m = drainInboxCmds(next, cmd)
		}
		if m.cron.jobPage.job != job || !m.cron.jobPage.ready || a.job != job {
			ht.Fatalf("visible job %q left without its own history: page=%+v read=%q", job, m.cron.jobPage, a.job)
		}
	}, hegel.WithTestCases(300))
}

// Review reproduction: refresh then repeated failed first-page retries must
// retain the same-job older rows until a successful replacement arrives.
func TestCronHistoryFailedRetriesRetainRows(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		m := cronFixture(t)
		job := m.cron.data.jobs[0].UID
		count := int(hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))%15) + 1
		rows := make([]generated.CronRun, count)
		for i := range rows {
			rows[i] = generated.CronRun{UID: fmt.Sprintf("old-%d", i), JobUID: new(job)}
		}
		m.cron.jobPage = cronHistoryPage{job: job, runs: rows, ready: true, older: true}
		a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, err: errors.New("offline")}
		m.api = a
		attempts := int(hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))%8) + 2
		for range attempts {
			var cmd tea.Cmd
			m, cmd = m.cronRequestPage(job, true)
			m = m.cronPageLoaded(cmd().(cronPageMsg))
			if len(m.cron.jobPage.runs) != len(rows) || m.cron.jobPage.runs[0].UID != rows[0].UID {
				ht.Fatalf("failed reset discarded rows: got %v want %v", m.cron.jobPage.runs, rows)
			}
		}
		a.err = nil
		m, cmd := m.cronRequestPage(job, true)
		m = m.cronPageLoaded(cmd().(cronPageMsg))
		if len(m.cron.jobPage.runs) != 0 || !m.cron.jobPage.ready {
			ht.Fatalf("successful empty replacement retained rows: %+v", m.cron.jobPage)
		}
	}, hegel.WithTestCases(300))
}

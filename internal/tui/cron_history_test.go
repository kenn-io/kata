package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
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

// A cursor-bearing server page is full; rows are newest first.
func cronHistoryRows(base generated.CronRun, count int) []generated.CronRun {
	rows := make([]generated.CronRun, count)
	for i := range rows {
		rows[i] = base
		if i > 0 {
			rows[i].UID = fmt.Sprintf("%s-%03d", base.UID, i)
			rows[i].CreatedAt = base.CreatedAt.Add(-time.Duration(i) * time.Minute)
		}
	}
	return rows
}

func TestCronHistoryFencesAndSingleFlight(t *testing.T) {
	m := cronFixture(t)
	j := m.cron.data.jobs[0].UID
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: cronHistoryRows(m.cron.data.runs[0], 20)}
	a.next = a.page[len(a.page)-1].UID
	m.api = a
	m.cron.jobPage = cronHistoryPage{job: j, next: a.next, ready: true}
	m, cmd := m.cronRequestPage(j, false)
	require.NotNil(t, cmd)
	_, second := m.cronRequestPage(j, false)
	require.Nil(t, second, "one outstanding read per history even with a continuation cursor")
	m, cmd = m.cronRequestPage(j, true)
	stale := cmd().(cronPageMsg)
	// A -> B -> A reopening has the same scope, job and cursor, but a new generation.
	m, _ = m.cronRequestPage("different", true)
	m, current := m.cronRequestPage(j, true)
	m = m.cronPageLoaded(stale)
	require.True(t, m.cron.jobPage.out)
	fresh := current().(cronPageMsg)
	for _, edit := range []func(*cronPageMsg){
		func(p *cronPageMsg) { p.conn++ }, func(p *cronPageMsg) { p.scope++ },
		func(p *cronPageMsg) { p.cursor = "wrong" }, func(p *cronPageMsg) { p.job = "wrong" },
	} {
		bad := fresh
		edit(&bad)
		next := m.cronPageLoaded(bad)
		require.True(t, next.cron.jobPage.out)
	}
	m = m.cronPageLoaded(fresh)
	require.False(t, m.cron.jobPage.out)
	require.Equal(t, a.next, m.cron.jobPage.next)
	for _, event := range []tea.Msg{keyRune('r'), resetRequiredMsg{}} {
		base := m
		base, cmd = base.cronRequestPage(j, false)
		late := cmd().(cronPageMsg)
		base, _ = updateModel(base, event)
		before := base.cron.jobPage
		before.runs = slices.Clone(before.runs)
		base = base.cronPageLoaded(late)
		require.Equal(t, before.out, base.cron.jobPage.out)
		require.Equal(t, before.runs, base.cron.jobPage.runs)
		require.Equal(t, before.next, base.cron.jobPage.next)
		require.NotEqual(t, late.gen, base.cron.jobPage.gen, "manual refresh/reset invalidates pages immediately")
	}
}

func TestCronHistoryFailureRetryAndForeignJob(t *testing.T) {
	m := cronFixture(t)
	j := m.cron.data.jobs[0].UID
	r := m.cron.data.runs[0]
	r.UID, r.Revision = "old-run", 1
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: cronHistoryRows(r, 20)}
	a.next = a.page[len(a.page)-1].UID
	cursor := a.next
	m.api = a
	m, cmd := m.cronRequestPage(j, true)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Equal(t, j, a.job)
	require.Equal(t, int64(20), a.limit)
	require.Empty(t, a.before)
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, j}}}
	_, _, lines := cronScreen(m)
	require.True(t, slices.ContainsFunc(lines, func(l cronLine) bool { return l.target == cronRef{cronRuns, r.UID} }))
	require.True(t, slices.ContainsFunc(lines, func(l cronLine) bool { return l.target.kind == cronOlder }))
	a.err = errors.New("offline")
	m, cmd = m.cronRequestPage(j, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Len(t, m.cron.jobPage.runs, 20)
	require.Equal(t, cursor, m.cron.jobPage.next)
	require.ErrorContains(t, m.cron.jobPage.err, "offline")
	a.err = nil
	a.page = []generated.CronRun{{UID: "foreign", JobUID: new("other")}}
	m, cmd = m.cronRequestPage(j, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.ErrorContains(t, m.cron.jobPage.err, "job")
	require.Equal(t, r.UID, m.cron.jobPage.runs[0].UID, "foreign pages cannot replace retained evidence")
	require.Equal(t, cursor, m.cron.jobPage.next)
	require.Len(t, m.cron.jobPage.runs, 20)
	older := r
	older.UID, older.CreatedAt = "older-run", r.CreatedAt.Add(-time.Hour)
	a.page = []generated.CronRun{older}
	a.next = ""
	m, cmd = m.cronRequestPage(j, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Equal(t, cursor, a.before)
	require.Len(t, m.cron.jobPage.runs, 21)
	m.cron.stack = append(m.cron.stack, cronFrame{ref: cronRef{cronRuns, r.UID}})
	require.Contains(t, stripANSI(m.viewBody()), r.Status)
	require.Contains(t, stripANSI(m.viewBody()), "runs / "+r.UID)
	m.cron.stack = m.cron.stack[:1]

	// A failed same-job reset keeps old rows; a successful empty reset clears them.
	m = cronFixture(t)
	job := m.cron.data.jobs[0].UID
	rows := []generated.CronRun{{UID: "old-run", JobUID: new(job)}}
	m.cron.jobPage = cronHistoryPage{job: job, runs: rows, ready: true, older: true}
	a = &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, err: errors.New("offline")}
	m.api = a
	m, cmd = m.cronRequestPage(job, true)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Equal(t, rows, m.cron.jobPage.runs, "failed reset retains every loaded row")
	require.ErrorContains(t, m.cron.jobPage.err, "offline")
	a.err = nil
	m, cmd = m.cronRequestPage(job, true)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Empty(t, m.cron.jobPage.runs, "successful empty replacement clears rows")
	require.True(t, m.cron.jobPage.ready)
}

func TestCronRunsLoadOlderByKey(t *testing.T) {
	m := sendRune(cronFixture(t), '3')
	m.cron.data.runs = cronHistoryRows(m.cron.data.runs[0], 100)
	cursor := m.cron.data.runs[len(m.cron.data.runs)-1].UID
	m.cron.data.nextBefore = cursor
	m.cron.runPage = cronHistoryPage{runs: m.cron.data.runs, next: cursor, ready: true}
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: []generated.CronRun{{UID: "old", Status: "failed"}}}
	m.api = a
	_, _, lines := cronScreen(m)
	m.cron.lists[cronRuns].cursor = len(lines) - 1
	m, cmd := m.routeCronKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Empty(t, a.job)
	require.Equal(t, cursor, a.before)
	require.Equal(t, int64(100), a.limit)
	title, _, _ := cronScreen(m)
	require.Contains(t, title, "101 loaded runs")
	require.NotContains(t, title, "newest 100")
}

func TestSnapshot_CronHistory(t *testing.T) {
	defer snapshotInit(t)()
	m := cronFixture(t)
	m.cron.runPage.runs = cronHistoryRows(m.cron.data.runs[0], 100)
	m.cron.runPage.next = m.cron.runPage.runs[len(m.cron.runPage.runs)-1].UID
	m = sendRune(m, '3')
	_, _, lines := cronScreen(m)
	m.cron.lists[cronRuns].cursor = len(lines) - 1 // Keep the continuation visible.
	assertGolden(t, "cron-runs-paging", m.viewBody())
	m.height = 60 // A full job page and its continuation must fit below the definition.
	j := m.cron.data.jobs[0].UID
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, j}}}
	rows := cronHistoryRows(m.cron.data.runs[0], 20)
	m.cron.jobPage = cronHistoryPage{job: j, runs: rows, next: rows[len(rows)-1].UID, ready: true}
	assertGolden(t, "cron-job-history", m.viewBody())
}

// A -> workflow -> B -> back to A must start A's
// history again; a single cached page cannot leave the visible job loading.
func TestCronHistoryBackToJobRestartsHistory(t *testing.T) {
	m := cronFixture(t)
	job := m.cron.data.jobs[0].UID
	other := "other-job"

	second := m.cron.data.jobs[0]
	second.UID = other
	m.cron.data.jobs = append(m.cron.data.jobs, second)
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}}
	m.api = a
	follow := func(target cronRef) {
		_, _, lines := cronScreen(m)
		i := slices.IndexFunc(lines, func(l cronLine) bool { return l.target == target })
		if i < 0 {
			t.Fatalf("missing reachable link %+v", target)
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
		t.Fatalf("visible job %q left without its own history: page=%+v read=%q", job, m.cron.jobPage, a.job)
	}
}

func TestCronHistoryRetainedRuns(t *testing.T) {
	for _, history := range []string{"project", "job", "lookup"} {
		t.Run(history, func(t *testing.T) {
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
			if history == "lookup" {
				// An inspected lookup can outlive both history pages.
				m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{head}, ready: true}
				m.cron.jobPage = cronHistoryPage{}
			}
			if history == "job" {
				m.cron.stack = append([]cronFrame{{ref: cronRef{cronJobs, *old.JobUID}}}, m.cron.stack...)
			}
			fresh := old
			fresh.Status, fresh.Revision = "succeeded", 2
			fresh.EndedAt = new(snapshotFixedNow)
			a := &cronRunRefreshAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, run: fresh}
			m.api = newUndoClient(a)
			m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
			got, ok := m.cron.data.run(old.UID)
			require.True(t, ok)
			require.Equal(t, "succeeded", got.Status, "detail refresh succeeds")
			if history != "lookup" {
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
				m.cron.stack = append(m.cron.stack, cronFrame{ref: cronRef{cronRuns, old.UID}})
			}
			a.err = errors.New("run refresh offline")
			m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
			_, _, lines := cronScreen(m)
			require.Contains(t, cronLinesText(lines), "run refresh offline")
		})
	}

	t.Run("opening retained failure", func(t *testing.T) {
		m := cronSeenFixture(t)
		head := m.cron.data.runs[0]
		r := head
		r.UID, r.Status = "retained-failure", "failed"
		r.CreatedAt = snapshotFixedNow.Add(-24 * time.Hour)
		r.EndedAt = new(r.CreatedAt)
		m.cron.data.runs = []generated.CronRun{head}
		m.cron.data.lastRuns = map[string]generated.CronRun{*head.JobUID: head}
		m.cron.runPage = cronHistoryPage{runs: []generated.CronRun{head, r}, ready: true, older: true}
		m = m.cronRemember([]generated.CronRun{r})
		// Refreshing the head must preserve this older failure in history.
		m.cron.runPage.older = true
		m.api = &cronReadTestAPI{data: *m.cron.data}
		m.cron.events++
		m = m.cronLoaded(m.fetchCron()().(cronLoadedMsg))
		m.cron.tab = cronRuns
		_, _, lines := cronScreen(m)
		i := slices.IndexFunc(lines, func(l cronLine) bool { return l.target == cronRef{cronRuns, r.UID} })
		require.NotEqual(t, -1, i)
		m.cron.lists[cronRuns].cursor = i
		m = sendKey(m, tea.KeyEnter)
		require.Zero(t, m.cronUnseenCount(), "opening the retained row acknowledges it")
		require.NotContains(t, cronScreenText(m), "no longer in the loaded data")
		require.Contains(t, cronScreenText(m), "failed")
	})

}

type cronRunRefreshAPI struct {
	*cronReadTestAPI
	run generated.CronRun
	err error
}

func (a *cronRunRefreshAPI) readCronRun(ctx context.Context, _ int64, uid string) (generated.CronRun, error) {
	if err := ctx.Err(); err != nil {
		return generated.CronRun{}, err
	}
	if uid != a.run.UID {
		return generated.CronRun{}, errors.New("wrong run UID")
	}
	return a.run, a.err
}

func cronGapFixture(t *testing.T, jobPage bool, count int) (Model, *cronReadTestAPI, string) {
	t.Helper()
	m := cronFixture(t)
	m.cron.tab = cronRuns
	base := m.cron.data.runs[0]
	runs := make([]generated.CronRun, count)
	for i := range count {
		runs[i] = base
		runs[i].UID = fmt.Sprintf("run-%04d", i)
		runs[i].CreatedAt = snapshotFixedNow.Add(time.Duration(i) * time.Minute)
	}
	a := &cronReadTestAPI{data: *m.cron.data}
	a.data.runs = runs
	m.api = a
	m.cron.data.runs = nil
	m.cron.runPage = cronHistoryPage{ready: true}
	job := ""
	if jobPage {
		job = *base.JobUID
		m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, job}}}
		m.cron.jobPage = cronHistoryPage{job: job, ready: true}
	}
	return m, a, job
}

func cronGapPage(m *Model, job string) *cronHistoryPage {
	if job != "" {
		return &m.cron.jobPage
	}
	return &m.cron.runPage
}

func cronGapEvent(t *testing.T, m Model) Model {
	t.Helper()
	model, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: 7})
	m = model.(Model)
	// Deliver the debounce tick directly; no wall-clock wait is needed.
	model, _ = m.handleRefetchTick()
	m = model.(Model)
	m, fetch := m.cronSync()
	require.NotNil(t, fetch)
	m = m.cronLoaded(fetch().(cronLoadedMsg))
	m, head := m.cronSync()
	if head != nil {
		m = m.cronPageLoaded(head().(cronPageMsg))
	}
	return m
}

func cronGapOlder(t *testing.T, m Model, job string) Model {
	t.Helper()
	_, _, lines := cronScreen(m)
	selected := slices.IndexFunc(lines, func(l cronLine) bool { return l.target.kind == cronOlder })
	require.GreaterOrEqual(t, selected, 0)
	if job == "" {
		m.cron.lists[cronRuns].cursor = selected
	} else {
		m.cron.stack[0].cursor = selected
	}
	m, request := m.routeCronKey(keyRune('l'))
	require.NotNil(t, request)
	msg := request().(cronPageMsg)
	require.NoError(t, msg.err)
	return m.cronPageLoaded(msg)
}

// Job details have independent server-filtered history; job pages and lookup
// never enlarge the workflow's documented newest-100 history.
func TestCronHistoryJobPagesAndLookup(t *testing.T) {
	m := cronFixture(t)
	j := m.cron.data.jobs[0]
	old := m.cron.data.runs[0]
	old.UID = "old-run"
	old.Revision = 1
	a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: cronHistoryRows(old, 20)}
	m.api = a
	m.cron.jobPage = cronHistoryPage{job: j.UID, runs: a.page, ready: true}
	m = m.cronRemember(a.page)
	m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, j.UID}}}
	m.cron.stack = append(m.cron.stack, cronFrame{ref: cronRef{cronRuns, old.UID}})
	require.Contains(t, stripANSI(m.viewBody()), old.Status)
	require.Contains(t, stripANSI(m.viewBody()), "runs / "+old.UID)
	// A head refresh must retain the run currently being inspected, including
	// when the project-head read fails.
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("inspected job run/failed=%v", failed), func(t *testing.T) {
			data := *m.cron.data
			head := old
			head.UID, head.CreatedAt = "current-head", old.CreatedAt.Add(24*time.Hour)
			data.runs = cronHistoryRows(head, 100)
			data.nextBefore = data.runs[len(data.runs)-1].UID
			if failed {
				data.runsErr = errors.New("offline")
			}
			next := m.cronLoaded(cronLoadedMsg(data))
			require.Contains(t, stripANSI(next.viewBody()), old.Status)
			require.Contains(t, stripANSI(next.viewBody()), "runs / "+old.UID)
		})
	}

	m.cron.stack = []cronFrame{{ref: cronRef{cronWorkflows, m.cron.data.workflows[0].UID}}}
	_, _, lines := cronScreen(m)
	require.False(t, slices.ContainsFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID }))
	require.Contains(t, cronLinesText(lines), fmt.Sprintf("Loaded runs (%d) · newest 100", len(m.cron.data.runs)))

	t.Run("project lookup survives failed refresh", func(t *testing.T) {
		m := cronFixture(t)
		r := m.cron.data.runs[0]
		r.UID = "old-page"
		a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, page: []generated.CronRun{r}}
		m.api = a
		m.cron.runPage.runs = cronHistoryRows(m.cron.data.runs[0], 100)
		m.cron.runPage.next = m.cron.runPage.runs[len(m.cron.runPage.runs)-1].UID
		m, cmd := m.cronRequestPage("", false)
		m = m.cronPageLoaded(cmd().(cronPageMsg))
		data := *m.cron.data
		data.runsErr = errors.New("offline")
		m = m.cronLoaded(cronLoadedMsg(data))
		m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, r.UID}}}
		_, _, lines := cronScreen(m)
		require.Contains(t, cronLinesText(lines), "Report ready")
	})
}

// Events retain pages, cursors, selection and in-flight reads; manual refresh
// resets pages. Failed refreshes keep evidence and expose the read error.
func TestCronHistoryEventPages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check func(*testing.T)
	}{
		{"refresh policy", func(t *testing.T) {
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
					m.cron.runPage = cronHistoryPage{runs: cronHistoryRows(old, 100), next: old.UID, ready: true, older: true}
					m.cron.jobPage = cronHistoryPage{job: *old.JobUID, runs: cronHistoryRows(old, 20), next: old.UID, ready: true, older: true}
					m.cron.runPage.next = m.cron.runPage.runs[len(m.cron.runPage.runs)-1].UID
					m.cron.jobPage.next = m.cron.jobPage.runs[len(m.cron.jobPage.runs)-1].UID
					projectCursor, jobCursor := m.cron.runPage.next, m.cron.jobPage.next
					m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, *old.JobUID}}}
					_, _, lines := cronScreen(m)
					m.cron.stack[0].cursor = slices.IndexFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID })
					require.GreaterOrEqual(t, m.cron.stack[0].cursor, 0)
					fresh := *m.cron.data
					fresh.runs = []generated.CronRun{head}
					if kind != "manual" {
						// A real head overlaps the retained pages and advertises a newer cursor.
						fresh.runs = append(fresh.runs, m.cron.runPage.runs[:99]...)
					}
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
					require.Equal(t, projectCursor, m.cron.runPage.next)
					require.Equal(t, jobCursor, m.cron.jobPage.next)
					_, _, lines = cronScreen(m)
					require.Equal(t, old.UID, lines[m.cron.stack[0].cursor].target.uid, "event keeps the selected run even when the head grows")
					if kind == "failed event" {
						require.ErrorContains(t, m.cron.runPage.err, "offline")
						require.ErrorContains(t, m.cron.jobPage.err, "offline")
					}
				})
			}
		}},
		{"head gap", func(t *testing.T) {
			for _, jobPage := range []bool{false, true} {
				limit := 100
				if jobPage {
					limit = 20
				}
				for _, arrivals := range []int{limit, 150} {
					for _, state := range []string{"deep", "exhausted", "empty"} {
						for _, selectOlder := range []bool{false, true} {
							if (state == "exhausted" || state == "empty") && selectOlder {
								continue // Identical to deep/link=true.
							}
							t.Run(fmt.Sprintf("job=%v/arrivals=%d/%s/link=%v", jobPage, arrivals, state, selectOlder), func(t *testing.T) {
								m, a, job := cronGapFixture(t, jobPage, arrivals+limit+1)
								p := cronGapPage(&m, job)
								if state != "empty" {
									p.runs = slices.Clone(a.data.runs[1 : limit+1])
									slices.Reverse(p.runs)
								}
								if state == "deep" || selectOlder {
									p.runs = slices.Clone(a.data.runs[1 : limit+1])
									slices.Reverse(p.runs)
									p.next = a.data.runs[1].UID
								}
								_, _, lines := cronScreen(m)
								wanted := cronRef{cronRuns, a.data.runs[1].UID}
								if selectOlder {
									wanted = cronRef{kind: cronOlder, uid: job}
									if job == "" {
										wanted.uid = "project"
									}
								}
								selected := slices.IndexFunc(lines, func(l cronLine) bool { return l.target == wanted })
								if state != "empty" || selectOlder {
									require.GreaterOrEqual(t, selected, 0)
								}
								if selected >= 0 {
									if jobPage {
										m.cron.stack[0].cursor = selected
									} else {
										m.cron.lists[cronRuns].cursor = selected
									}
								}
								m = cronGapEvent(t, m)
								p = cronGapPage(&m, job)
								require.Equal(t, a.data.runs[len(a.data.runs)-limit].UID, p.next)
								if selected >= 0 {
									_, _, lines = cronScreen(m)
									cursor := m.cron.lists[cronRuns].cursor
									if jobPage {
										cursor = m.cron.stack[0].cursor
									}
									require.Equal(t, wanted, lines[cursor].target)
								}
								m = cronGapOlder(t, m, job)
								p = cronGapPage(&m, job)
								require.True(t, slices.ContainsFunc(p.runs, func(r generated.CronRun) bool { return r.UID == a.data.runs[len(a.data.runs)-limit-1].UID }))
								if state != "empty" {
									require.Contains(t, p.runs, a.data.runs[1])
								}
							})
						}
					}
				}
			}
		}},
		{"inflight older page", func(t *testing.T) {
			for _, jobPage := range []bool{false, true} {
				limit := 100
				if jobPage {
					limit = 20
				}
				for _, olderFirst := range []bool{false, true} {
					for _, terminal := range []bool{false, true} {
						for _, overlaps := range []bool{false, true} {
							if terminal && overlaps {
								continue // overlaps overrides terminal: identical to terminal=false.
							}
							t.Run(fmt.Sprintf("job=%v/older-first=%v/terminal=%v/overlap=%v", jobPage, olderFirst, terminal, overlaps), func(t *testing.T) {
								m, a, job := cronGapFixture(t, jobPage, 350)
								index := 101
								if terminal {
									index = 1
								}
								if overlaps {
									index = 340
								}
								p := cronGapPage(&m, job)
								start, end := index, index+limit
								if overlaps {
									// Retain a full page ending at the original overlapping row.
									start, end = index-limit+1, index+1
								}
								p.runs, p.next = slices.Clone(a.data.runs[start:end]), a.data.runs[start].UID
								slices.Reverse(p.runs)
								m, request := m.cronRequestPage(job, false)
								late := request().(cronPageMsg)
								require.NoError(t, late.err)
								gen, cursor := cronGapPage(&m, job).gen, cronGapPage(&m, job).cursor
								if olderFirst {
									m = m.cronPageLoaded(late)
								}
								m = cronGapEvent(t, m)
								if !olderFirst {
									p = cronGapPage(&m, job)
									require.True(t, p.out)
									require.Equal(t, gen, p.gen)
									require.Equal(t, cursor, p.cursor)
									m = m.cronPageLoaded(late)
									// Job head reloads intentionally wait for older results.
									next, head := m.cronSync()
									m = next
									if head != nil {
										m = m.cronPageLoaded(head().(cronPageMsg))
									}
								}
								p = cronGapPage(&m, job)
								require.Contains(t, p.runs, a.data.runs[index])
								for _, r := range late.runs {
									require.Contains(t, p.runs, r)
								}
								require.False(t, p.out)
								want := a.data.runs[350-limit].UID
								if overlaps {
									want = late.next
								}
								require.Equal(t, want, p.next)
								m = cronGapOlder(t, m, job)
								require.Contains(t, cronGapPage(&m, job).runs, a.data.runs[350-limit-1])
							})
						}
					}
				}
			}
		}},
		{"successive gaps", func(t *testing.T) {
			for _, jobPage := range []bool{false, true} {
				t.Run(fmt.Sprintf("job=%v", jobPage), func(t *testing.T) {
					limit := 100
					if jobPage {
						limit = 20
					}
					m, a, job := cronGapFixture(t, jobPage, 6*limit)
					all := slices.Clone(a.data.runs)
					p := cronGapPage(&m, job)
					p.runs, p.next = slices.Clone(all[:limit]), all[0].UID
					slices.Reverse(p.runs)
					a.data.runs = all[:4*limit]
					m = cronGapEvent(t, m)
					m = cronGapOlder(t, m, job) // Fill only part of the first gap.
					a.data.runs = all
					m = cronGapEvent(t, m)
					presses := 0
					for cronGapPage(&m, job).next != "" {
						require.Less(t, presses, 10, "continuation must terminate")
						m = cronGapOlder(t, m, job)
						presses++
					}
					// The full final page advertises a cursor; the final empty reply clears it.
					require.Equal(t, 6, presses)
					got := cronGapPage(&m, job).runs
					require.Len(t, got, len(all))
					for i, r := range got {
						require.Equal(t, all[len(all)-1-i].UID, r.UID)
					}
				})
			}
		}},
	} {
		t.Run(tc.name, tc.check)
	}
}

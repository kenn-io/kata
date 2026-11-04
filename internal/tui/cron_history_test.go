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
	} {
		bad := fresh
		edit(&bad)
		next := m.cronPageLoaded(bad)
		require.True(t, next.cron.jobPage.out)
	}
	m = m.cronPageLoaded(fresh)
	require.False(t, m.cron.jobPage.out)
	require.Equal(t, "next", m.cron.jobPage.next)
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
	require.Equal(t, r.UID, m.cron.jobPage.runs[0].UID, "foreign pages cannot replace retained evidence")
	require.Equal(t, "next", m.cron.jobPage.next)
	require.Len(t, m.cron.jobPage.runs, 1)
	a.page = nil
	a.next = ""
	m, cmd = m.cronRequestPage(j, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.NoError(t, m.cron.jobPage.err)
	require.Empty(t, m.cron.jobPage.next, "empty terminal page is ordinary exhaustion")

	// Failed first-page retries retain every row until a successful empty reset.
	for count := 1; count <= 15; count++ {
		for attempts := 2; attempts <= 9; attempts++ {
			m := cronFixture(t)
			job := m.cron.data.jobs[0].UID
			rows := make([]generated.CronRun, count)
			for i := range rows {
				rows[i] = generated.CronRun{UID: fmt.Sprintf("old-%d", i), JobUID: new(job)}
			}
			m.cron.jobPage = cronHistoryPage{job: job, runs: rows, ready: true, older: true}
			a := &cronPagingAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, err: errors.New("offline")}
			m.api = a
			for range attempts {
				var cmd tea.Cmd
				m, cmd = m.cronRequestPage(job, true)
				m = m.cronPageLoaded(cmd().(cronPageMsg))
				require.Equal(t, rows, m.cron.jobPage.runs, "failed reset retains every loaded row")
			}
			a.err = nil
			m, cmd := m.cronRequestPage(job, true)
			m = m.cronPageLoaded(cmd().(cronPageMsg))
			if len(m.cron.jobPage.runs) != 0 || !m.cron.jobPage.ready {
				t.Fatalf("successful empty replacement retained rows: %+v", m.cron.jobPage)
			}
		}
	}
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

// A -> workflow -> B -> back to A must start A's
// history again; a single cached page cannot leave the visible job loading.
func TestCronHistoryBackToJobRestartsHistory(t *testing.T) {
	{
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
}

func TestCronHistoryRetainedRunRevision(t *testing.T) {
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
			m.api = &cronRunRefreshAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, run: fresh}
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

	t.Run("inspected lookup and failed refresh through undo client", func(t *testing.T) {
		m := cronFixture(t)
		old := generated.CronRun{UID: "old-run", Revision: 1, Status: "running", IssueUID: new("old-issue")}
		m.cron.data.lookup = map[string]generated.CronRun{old.UID: old}
		m.cron.stack = []cronFrame{{ref: cronRef{cronRuns, old.UID}}}
		fresh := old
		fresh.Revision = 2
		fresh.Status = "succeeded"
		a := &cronRunRefreshAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, run: fresh}
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

func TestCronHistoryRetainedRunLink(t *testing.T) {
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

func TestCronHistoryRefreshPreservesEventPages(t *testing.T) {
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

func TestCronHistoryEventKeepsInflightOlderPage(t *testing.T) {
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
	older.CreatedAt = old.CreatedAt.Add(-time.Hour)
	a.page = []generated.CronRun{older}
	a.next = ""
	m, cmd = m.cronRequestPage(j.UID, false)
	m = m.cronPageLoaded(cmd().(cronPageMsg))
	require.Equal(t, "older", a.before)
	require.Len(t, m.cron.jobPage.runs, 2)
	m.cron.stack = append(m.cron.stack, cronFrame{ref: cronRef{cronRuns, old.UID}})
	require.Contains(t, stripANSI(m.viewBody()), old.Status)
	require.Contains(t, stripANSI(m.viewBody()), "runs / "+old.UID)
	// A head refresh must retain the run currently being inspected.
	data := *m.cron.data
	data.runs = nil
	m = m.cronLoaded(cronLoadedMsg(data))
	require.Contains(t, stripANSI(m.viewBody()), old.Status)
	require.Contains(t, stripANSI(m.viewBody()), "runs / "+old.UID)
	m.cron.stack = []cronFrame{{ref: cronRef{cronWorkflows, m.cron.data.workflows[0].UID}}}
	_, _, lines = cronScreen(m)
	require.False(t, slices.ContainsFunc(lines, func(l cronLine) bool { return l.target.uid == old.UID }))

	t.Run("project lookup survives failed refresh", func(t *testing.T) {
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
	})
}

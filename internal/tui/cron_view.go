package tui

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/tui/splitlayout"
)

const (
	cronKata = iota
	cronJobs
	cronRuns
	cronWorkflows
)

type cronRef struct {
	kind int
	uid  string
}
type cronFrame struct {
	ref    cronRef
	cursor int
}
type cronList struct {
	cursor int
	query  string
}
type cronData struct {
	conn, scope, events uint64
	generation          uint64
	resetPages          bool
	jobs                []generated.CronJob
	workflows           []generated.CronWorkflow
	runs                []generated.CronRun
	nextBefore          string
	replica             cronReplica
	lastRuns            map[string]generated.CronRun
	lastErr             map[string]error
	lookup              map[string]generated.CronRun
	progress            map[string]cron.RecurringProgress
	parentOpen          map[string]bool
	waits               map[string]cronParent
	waitErr             map[string]error
	historyRuns         []generated.CronRun
	inspectedRuns       []generated.CronRun
	changed             []string
	deferred            bool
	runErr              map[string]error
	jobsErr             error
	workflowsErr        error
	runsErr             error
}
type cronLoadedMsg cronData

func (p cronData) readError(kind int) error {
	switch kind {
	case cronJobs:
		return p.jobsErr
	case cronRuns:
		return p.runsErr
	case cronWorkflows:
		return p.workflowsErr
	}
	return nil
}

type cronChange struct {
	at  uint64 // latest naming event covered by a fetch
	seq uint64 // first naming, or the back of the queue after a failed read
}

type cronState struct {
	changed          map[string]cronChange
	tab              int
	lists            [4]cronList
	filtering, help  bool
	returnToCron     bool
	issueReturn      *cronIssueReturn
	unseenOnly       bool
	stack            []cronFrame
	data             *cronData
	events, fence    uint64
	out              bool
	conn, scope      uint64
	form             *cronForm
	editSeq          uint64
	reads            *cronReadScope
	readGen          uint64
	resetPages       bool
	jobPage, runPage cronHistoryPage
}

// Synchronize after every update; events past fence wait for their debounce.
func (m Model) cronSync() (Model, tea.Cmd) {
	if m.cron.conn != m.connGen || m.cron.scope != m.scopeGen {
		m.cron.reads.stop()
		if m.modal == modalDiscardCronJob {
			m.modal = modalNone
		}
		m.cron = cronState{tab: m.cron.tab, conn: m.connGen, scope: m.scopeGen}
	}
	if m.view != viewCron || m.scope.allProjects || m.scope.projectID == 0 {
		m.cron.reads.stop()
		m.cron.reads = nil
		if m.cron.out || m.cron.jobPage.out || m.cron.runPage.out {
			m.cronInvalidatePages()
		}
		m.cron.readGen = 0
		m.cron.out = false
		return m, nil
	}
	if m.cron.out {
		return m, nil
	}
	if m.cron.reads == nil {
		// Update is a Bubble Tea framework callback with no supplied context.
		root := m.cronContext(context.Background())
		ctx, cancel := context.WithCancel(root)
		m.cron.reads = &cronReadScope{ctx: ctx, cancel: cancel}
	}
	if m.cron.data != nil && (m.cron.data.events == m.cron.events || m.pendingRefetch && m.cron.data.events >= m.cron.fence) {
		// Returning through another job can leave the sole page cache on that
		// job. Start a matching request as soon as this job becomes visible.
		if n := len(m.cron.stack); n > 0 {
			frame := m.cron.stack[n-1]
			if frame.ref.kind == cronJobs && m.cron.jobPage.job != frame.ref.uid {
				return m.cronRequestPage(frame.ref.uid, true)
			}
		}
		if m.cron.jobPage.reload && !m.cron.jobPage.out {
			for _, frame := range m.cron.stack {
				if frame.ref.kind == cronJobs && frame.ref.uid == m.cron.jobPage.job {
					return m.cronRequestPage(frame.ref.uid, true)
				}
			}
			m.cron.jobPage.reload = false
		}
		return m, nil
	}
	m.cron.out = true
	m.cron.readGen = m.cronGeneration()
	return m, m.fetchCron()
}
func (m Model) fetchCron() tea.Cmd {
	reader, id := m.api, m.scope.projectID
	data := cronData{conn: m.connGen, scope: m.scopeGen, events: m.cron.events, generation: m.cron.readGen}
	data.historyRuns = m.cronLoadedRuns()
	data.changed = slices.SortedFunc(maps.Keys(m.cron.changed), func(a, b string) int {
		return cmp.Or(cmp.Compare(m.cron.changed[a].seq, m.cron.changed[b].seq), strings.Compare(a, b))
	})
	data.resetPages = m.cron.resetPages
	if m.cron.data != nil {
		for _, frame := range m.cron.stack {
			if frame.ref.kind == cronRuns {
				if r, ok := m.cron.data.run(frame.ref.uid); ok {
					data.inspectedRuns = append(data.inspectedRuns, r)
				}
			}
		}
	}
	return func() tea.Msg {
		// Bubble Tea command callbacks supply no context in isolated model tests.
		root := m.cronContext(context.Background())
		if m.cron.reads != nil {
			root = m.cron.reads.ctx
		}
		ctx, cancel := context.WithTimeout(root, 3*defaultHTTPTimeout)
		defer cancel()
		cronFetch(ctx, reader, id, &data)
		return cronLoadedMsg(data)
	}
}
func (m Model) cronLoaded(msg cronLoadedMsg) Model {
	if msg.conn != m.connGen || msg.scope != m.scopeGen || (msg.generation != 0 && msg.generation != m.cron.readGen) {
		return m
	}
	data := cronData(msg)
	data.lastRuns = maps.Clone(data.lastRuns)
	if previous := m.cron.data; previous != nil {
		if data.jobsErr != nil {
			data.jobs = previous.jobs
		}
		for uid, run := range previous.lastRuns {
			if _, fresh := data.lastRuns[uid]; !fresh && (data.jobsErr != nil || data.lastErr[uid] != nil) {
				if data.lastRuns == nil {
					data.lastRuns = map[string]generated.CronRun{}
				}
				data.lastRuns[uid] = run
			}
		}
		if data.workflowsErr != nil {
			data.workflows = previous.workflows
		}
		if data.runsErr != nil {
			data.runs = previous.runs
		}
	}
	old := m
	m.cron.out, m.cron.data = false, &data
	if data.runsErr == nil {
		if data.resetPages || !m.cron.runPage.ready {
			m.cron.runPage = cronHistoryPage{runs: data.runs, next: data.nextBefore, ready: true, gen: m.cronGeneration()}
		} else {
			if data.nextBefore != "" && !cronRunsOverlap(m.cron.runPage.runs, data.runs) {
				m.cron.runPage.next = data.nextBefore
			}
			m.cron.runPage.runs = cronMergeRuns(m.cron.runPage.runs, data.runs)
			m.cron.runPage.err = nil
		}
		if data.resetPages && data.events == m.cron.events {
			m.cron.resetPages = false
		}
	} else {
		m.cron.runPage.err = data.runsErr
	}
	// Reset drops unread history, including an inactive job's cache.
	if data.resetPages && data.runsErr == nil && !slices.ContainsFunc(m.cron.stack, func(f cronFrame) bool {
		return f.ref.kind == cronJobs && f.ref.uid == m.cron.jobPage.job
	}) {
		m.cron.jobPage = cronHistoryPage{}
	}
	if m.cron.jobPage.job != "" {
		m.cron.jobPage.reload = true
		m.cron.jobPage.merge = !data.resetPages
	}
	// Retained pages and open run frames keep the highest known observation.
	retained := cronMergeRuns(m.cron.runPage.runs, m.cron.jobPage.runs)
	remembered := slices.Collect(maps.Values(data.lastRuns))
	if previous := old.cron.data; previous != nil {
		for _, frame := range m.cron.stack {
			if r, ok := previous.run(frame.ref.uid); ok && frame.ref.kind == cronRuns {
				if fresh, ok := data.lookup[r.UID]; !ok || r.Revision > fresh.Revision {
					remembered = append([]generated.CronRun{r}, remembered...)
				}
			}
		}
		for i, r := range retained {
			if known, ok := previous.run(r.UID); ok && known.Revision > r.Revision {
				retained[i] = known
			}
		}
	}
	m = m.cronRemember(append(remembered, retained...))
	m = m.cronSettleChanged(data).cronMarkOpenSeen()
	return m.cronKeepSelection(old)
}

// Event payload revisions belong to source peers, not this store's CAS sequence.
func (m Model) cronMarkChanged(msg eventReceivedMsg) Model {
	if msg.eventType != "cron.run.observed" || msg.cronRun == nil || m.cron.data == nil {
		return m
	}
	uid := msg.cronRun.UID
	if _, loaded := m.cron.data.run(uid); !loaded {
		return m
	}
	m.cron.changed = maps.Clone(m.cron.changed)
	if m.cron.changed == nil {
		m.cron.changed = map[string]cronChange{}
	}
	c, seen := m.cron.changed[uid]
	if !seen {
		c.seq = m.cron.events
	}
	c.at = m.cron.events
	m.cron.changed[uid] = c
	return m
}

// Successful reads clear only events captured by this fetch. Failures move to
// the back. Progress with deferred marks schedules one bounded follow-up;
// a refresh that clears nothing never schedules itself again.
func (m Model) cronSettleChanged(data cronData) Model {
	m.cron.changed = maps.Clone(m.cron.changed)
	cleared := false
	for uid, c := range m.cron.changed {
		err, read := data.runErr[uid]
		fresh := data.runsErr == nil && slices.ContainsFunc(data.runs, func(r generated.CronRun) bool { return r.UID == uid })
		_, loaded := m.cron.data.run(uid)
		switch {
		case !loaded:
			delete(m.cron.changed, uid)
		case c.at > data.events: // named after this fetch captured its inputs
		case fresh || read && err == nil:
			delete(m.cron.changed, uid)
			cleared = true
		case read:
			c.seq = m.cron.events + 1
			m.cron.changed[uid] = c
		}
	}
	if cleared && data.deferred {
		m.cron.events++
	}
	return m
}

func cronListLines(m Model, tab int) []cronLine {
	m.cron.tab, m.cron.stack = tab, nil
	_, _, lines := cronScreen(m)
	return lines
}
func (m Model) leaveCron() Model {
	m.view = m.prevView
	m.focus = focusList
	if m.view == viewDetail {
		m.focus = focusDetail
	}
	return m
}
func (m Model) switchCronTab(key string) Model {
	tab := int(key[0] - '1')
	if m.view != viewCron {
		if tab == cronKata {
			return m
		}
		m.prevView = m.view
		if m.layout == splitlayout.Split && m.focus == focusDetail {
			m.prevView = viewDetail
		}
		m.view = viewCron
	}
	if tab == cronKata {
		return m.leaveCron()
	}
	m.cron.tab, m.cron.stack, m.cron.filtering = tab, nil, false
	return m
}
func (m *Model) cronCursor() *int {
	if n := len(m.cron.stack); n > 0 {
		return &m.cron.stack[n-1].cursor
	}
	return &m.cron.lists[m.cron.tab].cursor
}

func (m Model) cronFilter(text string) Model {
	m.cron.lists[m.cron.tab].query = sanitizeForLine(text)
	m.cron.lists[m.cron.tab].cursor = 0
	return m
}

func (m Model) routeCronKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	key := msg.String()
	if m.cron.form != nil {
		return m.routeCronFormKey(msg)
	}
	if m.cron.help {
		if key == "esc" || key == "h" {
			m.cron.help = false
		}
		return m, nil
	}
	if m.cron.filtering {
		query := m.cron.lists[m.cron.tab].query
		switch key {
		case "ctrl+c":
			m.detail.cancelReads()
			return m, tea.Quit
		case "enter":
			m.cron.filtering = false
		case "esc":
			m.cron.filtering = false
			m = m.cronFilter("")
		case "backspace":
			r := []rune(query)
			m = m.cronFilter(string(r[:max(len(r)-1, 0)]))
		default:
			m = m.cronFilter(query + msg.Text)
		}
		return m, nil
	}
	_, _, lines := cronScreen(m)
	cursor := m.cronCursor()
	*cursor = min(max(*cursor, 0), max(len(lines)-1, 0))
	switch {
	case m.keymap.Up.matches(msg), m.keymap.ScrollUp.matches(msg):
		*cursor--
	case m.keymap.Down.matches(msg), m.keymap.ScrollDown.matches(msg):
		*cursor++
	case m.keymap.Home.matches(msg):
		*cursor = 0
	case m.keymap.End.matches(msg):
		*cursor = len(lines) - 1
	case m.keymap.PageUp.matches(msg):
		*cursor -= max(m.height-8, 1)
	case m.keymap.PageDown.matches(msg):
		*cursor += max(m.height-8, 1)
	case key == "n", key == "e":
		next, cmd, handled := m.routeCronJobKey(key, lines, *cursor)
		if handled {
			return next, cmd
		}
	case key == "enter" || key == "l":
		if len(lines) > 0 {
			target := lines[*cursor].target
			if target.kind == cronIssue {
				return m.cronOpenIssue(target.uid)
			}
			if target.kind == cronOlder {
				job := target.uid
				if job == "project" {
					job = ""
				}
				reset := job != "" && !m.cron.jobPage.ready && m.cron.jobPage.next == ""
				return m.cronRequestPage(job, reset)
			}
			if target.uid != "" {
				m.cron.stack = append(m.cron.stack, cronFrame{ref: target})
				if target.kind == cronRuns {
					m = m.cronOpenRun(target.uid)
				}
				if target.kind == cronJobs {
					return m.cronRequestPage(target.uid, true)
				}
			}
		}
	case key == "h" || key == "esc":
		if len(m.cron.stack) > 0 {
			m.cron.stack = m.cron.stack[:len(m.cron.stack)-1]
		} else if key == "esc" && m.cron.lists[m.cron.tab].query != "" {
			m = m.cronFilter("")
		} else {
			m = m.leaveCron()
		}
	case key == "/" && len(m.cron.stack) == 0:
		m.cron.filtering = true
	case key == "u":
		m.cron.tab = cronRuns
		m.cron.stack = nil
		m.cron.unseenOnly = !m.cron.unseenOnly || m.toast != nil && m.toast.cronFailure
		m.cron.lists[cronRuns].cursor = 0
	case key == "U":
		m = m.cronClearSeen()
	case key == "r":
		m.cron.resetPages, m.cron.fence = true, m.cron.events+1
		m.cronInvalidatePages()
		m.cron.events++
	}
	*cursor = min(max(*cursor, 0), max(len(lines)-1, 0))
	return m, nil
}

func (p cronData) job(uid string) *generated.CronJob {
	for _, j := range p.jobs {
		if j.UID == uid {
			return &j
		}
	}
	return nil
}
func (p cronData) workflow(uid string) *generated.CronWorkflow {
	for _, w := range p.workflows {
		if w.UID == uid {
			return &w
		}
	}
	return nil
}
func cronDefinitionName(p cronData, uid string, workflow bool) string {
	if workflow {
		if w := p.workflow(uid); w != nil {
			return w.Name
		}
	} else if j := p.job(uid); j != nil {
		return j.Name
	}
	return uid
}

func (p cronData) workflowJobs(uid string) []generated.CronJob {
	var jobs []generated.CronJob
	for _, j := range p.jobs {
		if deref(j.Definition.Action.WorkflowUID) == uid {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

func cronMatches(text, query string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(query))
}

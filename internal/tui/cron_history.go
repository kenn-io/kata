package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronOlder = -1

type cronHistoryPage struct {
	job, next, cursor         string
	gen                       uint64
	runs                      []generated.CronRun
	out, ready, older, reload bool
	merge                     bool
	err                       error
}

type cronPageMsg struct {
	conn, scope, gen  uint64
	events            uint64 // m.cron.events when requested
	job, cursor, next string
	merge             bool
	runs              []generated.CronRun
	err               error
}

func (m *Model) cronGeneration() uint64 { m.nextCronRequest++; return m.nextCronRequest }

// Invalidate at explicit refresh/reset or navigation, before an old page can arrive. The monotonic
// counter lives outside cronState so scope changes and A/B/A opens cannot reuse it.
func (m *Model) cronInvalidatePages() {
	m.cron.jobPage.gen = m.cronGeneration()
	m.cron.jobPage.out = false
	m.cron.jobPage.reload = m.cron.jobPage.job != ""
	m.cron.runPage.gen = m.cronGeneration()
	m.cron.runPage.out = false
}

func (m Model) cronRequestPage(job string, reset bool) (Model, tea.Cmd) {
	before := m
	page := &m.cron.runPage
	limit := int64(100)
	if job != "" {
		page = &m.cron.jobPage
		limit = 20
	}
	if !reset && (page.out || page.next == "") {
		return m, nil
	}
	merge := reset && page.merge && page.ready && page.job == job
	if reset && page.job != job {
		var runs []generated.CronRun
		if m.cron.data != nil {
			for _, r := range m.cron.data.runs {
				if job == "" || deref(r.JobUID) == job {
					runs = append(runs, r)
				}
			}
		}
		*page = cronHistoryPage{job: job, runs: runs}
	} else if reset {
		page.err, page.merge = nil, false
	}
	page.gen = m.cronGeneration()
	page.cursor = page.next
	if reset {
		page.cursor = ""
	}
	page.out, page.reload = true, false
	msg := cronPageMsg{conn: m.connGen, scope: m.scopeGen, gen: page.gen, events: m.cron.events, job: job, cursor: page.cursor, merge: merge}
	reader, id := m.api, m.scope.projectID
	m = m.cronKeepSelection(before)
	return m, func() tea.Msg {
		// Bubble Tea commands supply no context; production uses the TUI lifetime.
		root := m.cronContext(context.Background())
		if m.cron.reads != nil {
			root = m.cron.reads.ctx
		}
		ctx, cancel := context.WithTimeout(root, defaultHTTPTimeout)
		defer cancel()
		msg.runs, msg.next, msg.err = reader.ListCronJobRuns(ctx, id, job, msg.cursor, limit)
		if msg.err == nil && job != "" {
			msg.err = cronValidateJobPage(msg.runs, job)
		}
		return msg
	}
}

func (m Model) cronPageLoaded(msg cronPageMsg) Model {
	if msg.conn != m.connGen || msg.scope != m.scopeGen {
		return m
	}
	page := &m.cron.runPage
	if msg.job != "" {
		page = &m.cron.jobPage
	}
	if !page.out || msg.gen != page.gen || msg.job != page.job || msg.cursor != page.cursor {
		return m
	}
	previous := m
	page.out, page.err = false, msg.err
	if msg.err != nil {
		return m.cronKeepSelection(previous)
	}
	if msg.cursor == "" && !msg.merge {
		page.runs, page.older = nil, false
	} else if msg.cursor != "" {
		page.older = true
	}
	if msg.merge && msg.next != "" && !cronRunsOverlap(page.runs, msg.runs) {
		page.next = msg.next
	}
	page.runs = cronMergeRuns(page.runs, msg.runs)
	// An event may have moved continuation while this older read was in flight.
	if !msg.merge && (msg.cursor == "" || page.next == msg.cursor) {
		page.next = msg.next
		if len(msg.runs) == 0 || msg.next == msg.cursor {
			page.next = ""
		}
	}
	page.ready = true
	m = m.cronRemember(page.runs).cronNoticePage(msg)
	return m.cronKeepSelection(previous)
}

// A head that shares no loaded row may sit above unloaded runs.
func cronRunsOverlap(loaded, head []generated.CronRun) bool {
	return slices.ContainsFunc(head, func(r generated.CronRun) bool {
		return slices.ContainsFunc(loaded, func(l generated.CronRun) bool { return l.UID == r.UID })
	})
}

// Page overlap must not duplicate a row or downgrade an observation revision.
func cronMergeRuns(before, after []generated.CronRun) []generated.CronRun {
	out := slices.Clone(before)
	index := map[string]int{}
	for i, r := range out {
		index[r.UID] = i
	}
	for _, r := range after {
		if i, ok := index[r.UID]; ok {
			if r.Revision >= out[i].Revision {
				out[i] = r
			}
		} else {
			index[r.UID] = len(out)
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b generated.CronRun) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.UID, a.UID)
	})
	return out
}

// Preserve link selection when refreshed heads or error rows shift a document.
func (m Model) cronKeepSelection(before Model) Model {
	keep := func(old, fresh []cronLine, cursor int) int {
		if len(old) == 0 {
			return cursor
		}
		target := old[min(max(cursor, 0), len(old)-1)].target
		if target.uid != "" {
			if i := slices.IndexFunc(fresh, func(l cronLine) bool { return l.target == target }); i >= 0 {
				return i
			}
		}
		return min(max(cursor, 0), max(len(fresh)-1, 0))
	}
	for tab := cronJobs; tab <= cronWorkflows; tab++ {
		m.cron.lists[tab].cursor = keep(cronListLines(before, tab), cronListLines(m, tab), before.cron.lists[tab].cursor)
	}
	m.cron.stack = slices.Clone(m.cron.stack)
	for i := range m.cron.stack {
		if i >= len(before.cron.stack) || before.cron.stack[i].ref != m.cron.stack[i].ref {
			continue
		}
		a, b := before, m
		a.cron.stack = a.cron.stack[:i+1]
		b.cron.stack = b.cron.stack[:i+1]
		_, _, old := cronScreen(a)
		_, _, fresh := cronScreen(b)
		m.cron.stack[i].cursor = keep(old, fresh, before.cron.stack[i].cursor)
	}
	return m
}

func (m Model) cronRemember(runs []generated.CronRun) Model {
	if m.cron.data == nil {
		return m
	}
	data := *m.cron.data
	data.lookup = maps.Clone(data.lookup)
	if data.lookup == nil {
		data.lookup = map[string]generated.CronRun{}
	}
	for _, r := range runs {
		if old, ok := data.lookup[r.UID]; !ok || r.Revision >= old.Revision {
			data.lookup[r.UID] = r
		}
	}
	m.cron.data = &data
	return m
}

func (p cronData) run(uid string) (generated.CronRun, bool) {
	r, ok := p.lookup[uid]
	for _, candidate := range p.runs {
		if candidate.UID == uid && (!ok || candidate.Revision >= r.Revision) {
			r, ok = candidate, true
		}
	}
	return r, ok
}

func cronJobHistory(d *cronDoc, m Model, job string) {
	p := m.cron.jobPage
	if p.job != job {
		d.section("Runs · loading…")
		return
	}
	if p.ready && len(p.runs) == 0 {
		d.section("Runs")
	} else {
		d.section(fmt.Sprintf("Runs (%d)", len(p.runs)))
	}
	if p.out {
		d.text("loading…", cronRef{})
	}
	if p.err != nil {
		d.text("unavailable: "+p.err.Error(), cronRef{})
	}
	if len(p.runs) == 0 && p.ready {
		d.text("no runs", cronRef{})
	}
	for _, r := range p.runs {
		if current, ok := m.cron.data.run(r.UID); ok {
			r = current
		}
		d.text(cronRunStatus(*m.cron.data, r)+" · "+r.CreatedAt.Format("2006-01-02T15:04:05Z07:00")+" · "+deref(r.Summary.Message), cronRef{cronRuns, r.UID})
	}
	if p.next != "" {
		d.text("load 20 older runs", cronRef{cronOlder, job})
	} else if p.err != nil && !p.ready {
		d.text("retry runs", cronRef{cronOlder, job})
	}
}

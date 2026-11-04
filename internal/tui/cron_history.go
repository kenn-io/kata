package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronOlder = -1

type cronHistoryPage struct {
	job, next, cursor         string
	gen                       uint64
	runs                      []generated.CronRun
	out, ready, older, reload bool
	err                       error
}

type cronPageMsg struct {
	conn, scope, events, gen uint64
	job, cursor, next        string
	runs                     []generated.CronRun
	err                      error
}

func (m *Model) cronGeneration() uint64 { m.nextCronRequest++; return m.nextCronRequest }

// Invalidate at event/refresh time, before an old page can arrive. The monotonic
// counter lives outside cronState so scope changes and A/B/A opens cannot reuse it.
func (m *Model) cronInvalidatePages() {
	m.cron.jobPage.gen = m.cronGeneration()
	m.cron.jobPage.out = false
	m.cron.jobPage.reload = m.cron.jobPage.job != ""
	m.cron.runPage.gen = m.cronGeneration()
	m.cron.runPage.out = false
}

func (m Model) cronRequestPage(job string, reset bool) (Model, tea.Cmd) {
	page := &m.cron.runPage
	limit := int64(100)
	if job != "" {
		page = &m.cron.jobPage
		limit = 20
	}
	if !reset && (page.out || page.next == "") {
		return m, nil
	}
	if reset {
		runs := page.runs
		if page.job != job {
			runs = nil
			if m.cron.data != nil {
				for _, r := range m.cron.data.runs {
					if job == "" || deref(r.JobUID) == job {
						runs = append(runs, r)
					}
				}
			}
		}
		*page = cronHistoryPage{job: job, runs: runs}
	}
	page.gen = m.cronGeneration()
	page.cursor = page.next
	page.out, page.reload = true, false
	msg := cronPageMsg{conn: m.connGen, scope: m.scopeGen, events: m.cron.events, gen: page.gen, job: job, cursor: page.cursor}
	reader, id := m.api, m.scope.projectID
	return m, func() tea.Msg {
		// Bubble Tea commands supply no context; production uses the TUI lifetime.
		root := context.Background()
		if m.lifetime != nil {
			root = m.lifetime.ctx
		}
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
	if msg.conn != m.connGen || msg.scope != m.scopeGen || msg.events != m.cron.events {
		return m
	}
	page := &m.cron.runPage
	if msg.job != "" {
		page = &m.cron.jobPage
	}
	if !page.out || msg.gen != page.gen || msg.job != page.job || msg.cursor != page.cursor {
		return m
	}
	page.out, page.err = false, msg.err
	if msg.err != nil {
		return m
	}
	if msg.job != "" {
		if err := cronValidateJobPage(msg.runs, msg.job); err != nil {
			page.err = err
			return m
		}
	}
	if msg.cursor == "" {
		page.runs = cronMergeRuns(nil, msg.runs)
	} else {
		page.runs = cronMergeRuns(page.runs, msg.runs)
		page.older = true
	}
	page.next = msg.next
	if len(msg.runs) == 0 || msg.next == msg.cursor {
		page.next = ""
	}
	page.ready = true
	m = m.cronRemember(page.runs)
	return m
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
	return out
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
		d.text(cronRunStatus(*m.cron.data, r)+" · "+r.CreatedAt.Format("2006-01-02T15:04:05Z07:00")+" · "+deref(r.Summary.Message), cronRef{cronRuns, r.UID})
	}
	if p.next != "" {
		d.text("load 20 older runs", cronRef{cronOlder, job})
	} else if p.err != nil && !p.ready {
		d.text("retry runs", cronRef{cronOlder, job})
	}
}

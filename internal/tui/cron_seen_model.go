package tui

import (
	"context"
	"maps"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronSeenState struct {
	data                  cronSeenStore
	session               map[string]map[string]bool
	path                  string
	ready, writing, dirty bool
	err                   error
	pending               *cronSeenPending
}

// Local ids are held only until the catalog supplies the canonical UID. Scope
// fencing prevents a delayed catalog from acknowledging another selection.
type cronSeenPending struct {
	conn, scope    uint64
	projectID      int64
	runs           []string
	clearedThrough time.Time
}
type cronSeenResultMsg struct {
	data  cronSeenStore
	path  string
	write bool
	err   error
}

func (m Model) cronSeenCommand(write bool) tea.Cmd {
	snapshot, path := m.cronSeen.data, m.cronSeen.path
	return func() tea.Msg {
		// Bubble Tea supplies no command context; use the owned TUI lifetime.
		root := m.cronContext(context.Background())
		ctx, cancel := context.WithTimeout(root, defaultHTTPTimeout)
		defer cancel()
		msg := cronSeenResultMsg{path: path, write: write}
		if msg.path == "" {
			msg.path, msg.err = cronSeenPathForTUI()
		}
		if msg.err == nil {
			if write {
				msg.data, msg.err = cronSeenSave(ctx, msg.path, snapshot)
			} else {
				msg.data, msg.err = cronSeenLoad(ctx, msg.path)
			}
		}
		return msg
	}
}
func (m Model) cronSeenSync() (Model, tea.Cmd) {
	if p := m.cronSeen.pending; p != nil {
		if p.conn != m.connGen || p.scope != m.scopeGen || p.projectID != m.scope.projectID {
			m.cronSeen.pending = nil
		} else if project := m.cronProjectUID(); project != "" {
			m = m.cronApplySeen(project, p.runs, p.clearedThrough)
			m.cronSeen.pending = nil
		}
	}
	// Init owns the single startup read. Update only persists changes once
	// that read resolves, and cannot accidentally start a second load.
	if m.cronSeen.err != nil || !m.cronSeen.ready || !m.cronSeen.dirty || m.cronSeen.writing {
		return m, nil
	}
	m.cronSeen.dirty = false
	m.cronSeen.writing = true
	return m, m.cronSeenCommand(true)
}
func (m Model) cronSeenResult(msg cronSeenResultMsg) (Model, tea.Cmd) {
	if msg.write {
		m.cronSeen.writing = false
	} else {
		m.cronSeen.ready = true
	}
	if msg.err != nil {
		m.cronSeen.err = msg.err
		return m.cronNoticeArm()
	}
	if msg.path != "" {
		m.cronSeen.path = msg.path
	}
	m.cronSeen.data = cronSeenMerge(m.cronSeen.data, msg.data)
	return m.cronNoticeArm()
}
func (m Model) cronProjectUID() string { return m.projectUIDByID[m.scope.projectID] }

// Include every loaded source, preferring a higher revision, without changing
// the separate head/health provenance owned by cronData.
func (m Model) cronLoadedRuns() []generated.CronRun {
	if m.cron.data == nil {
		return nil
	}
	var runs []generated.CronRun
	// Apply earlier sources last to retain their equal-revision preference.
	for _, source := range [][]generated.CronRun{m.cron.runPage.runs, m.cron.jobPage.runs, slices.Collect(maps.Values(m.cron.data.lookup)), slices.Collect(maps.Values(m.cron.data.lastRuns)), m.cron.data.runs} {
		runs = cronMergeRuns(runs, source)
	}
	return slices.DeleteFunc(runs, func(r generated.CronRun) bool { return r.UID == "" })
}

func (m Model) cronRunUnseen(project string, r generated.CronRun) bool {
	if !m.cronSeen.ready || m.cronSeen.err != nil || project == "" || m.cronSeen.session[project][r.UID] {
		return false
	}
	return m.cronSeen.data.Projects[project].unseen(r)
}
func (m Model) cronUnseenCount() int {
	n := 0
	for _, r := range m.cronLoadedRuns() {
		if m.cronRunUnseen(m.cronProjectUID(), r) {
			n++
		}
	}
	return n
}
func (m Model) cronMarkSeen(uid string) Model {
	project := m.cronProjectUID()
	for _, r := range m.cronLoadedRuns() {
		if r.UID != uid || (r.Status != "failed" && r.Status != "unknown") {
			continue
		}
		m = m.cronApplySeen(project, []string{uid}, time.Time{})
		break
	}
	return m
}
func (m Model) cronApplySeen(project string, runs []string, cleared time.Time) Model {
	if project == "" {
		p := cronSeenPending{conn: m.connGen, scope: m.scopeGen, projectID: m.scope.projectID}
		if old := m.cronSeen.pending; old != nil && old.conn == p.conn && old.scope == p.scope && old.projectID == p.projectID {
			p.runs = slices.Clone(old.runs)
			p.clearedThrough = old.clearedThrough
		}
		p.runs = append(p.runs, runs...)
		if cleared.After(p.clearedThrough) {
			p.clearedThrough = cleared
		}
		m.cronSeen.pending = &p
		return m
	}
	session := maps.Clone(m.cronSeen.session)
	if session == nil {
		session = map[string]map[string]bool{}
	}
	marks := maps.Clone(session[project])
	if marks == nil {
		marks = map[string]bool{}
	}
	for _, uid := range runs {
		marks[uid] = true
	}
	session[project] = marks
	m.cronSeen.session = session
	delta := cronSeenStore{Projects: map[string]cronSeenProject{project: {Runs: runs, ClearedThrough: cleared}}}
	m.cronSeen.data = cronSeenMerge(m.cronSeen.data, delta)
	m.cronSeen.dirty = true
	return m
}
func (m Model) cronClearSeen() Model {
	project := m.cronProjectUID()
	var newest time.Time
	for _, r := range m.cronLoadedRuns() {
		if r.Status == "failed" || r.Status == "unknown" {
			if at := cronFailureTime(r); at.After(newest) {
				newest = at
			}
		}
	}
	if newest.IsZero() {
		return m
	}
	// Retain marks for runs outside the loaded window, whose time is unknown.
	return m.cronApplySeen(project, nil, newest)
}

// Matching counters name no run, so an open requests the fenced refresh that
// reads open runs by UID, as an explicit refresh does, before marking it seen.
func (m Model) cronOpenRun(uid string) Model {
	m.cron.events++
	m.cron.fence = m.cron.events
	return m.cronMarkSeen(uid)
}

// A refreshed failure in the open run is seen once shown; a closed one is not.
func (m Model) cronMarkOpenSeen() Model {
	n := len(m.cron.stack)
	if n == 0 || m.cron.stack[n-1].ref.kind != cronRuns || m.cronSeen.session[m.cronProjectUID()][m.cron.stack[n-1].ref.uid] {
		return m
	}
	return m.cronMarkSeen(m.cron.stack[n-1].ref.uid)
}

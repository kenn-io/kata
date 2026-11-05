package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronNoticeWindow = 2 * time.Second

type cronRunEvent struct {
	seq        uint64     // m.cron.events when this event arrived; reads that captured it describe it
	UID        string     `json:"uid"`
	JobUID     string     `json:"job_uid"`
	ProjectUID string     `json:"project_uid"`
	Status     string     `json:"status"`
	Revision   int64      `json:"revision"`
	EndedAt    *time.Time `json:"ended_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (r cronRunEvent) run() generated.CronRun {
	return generated.CronRun{UID: r.UID, Status: r.Status, Revision: r.Revision, EndedAt: r.EndedAt, UpdatedAt: r.UpdatedAt}
}

type cronNoticeState struct {
	conn, scope, gen   uint64
	window             bool
	pending, announced map[string]cronRunEvent
}
type cronNoticeTickMsg struct{ conn, scope, gen uint64 }

func (m Model) cronNoticeSync() Model {
	if m.cronNotices.conn != m.connGen || m.cronNotices.scope != m.scopeGen {
		m = m.cronNoticeReset()
	}
	return m
}

// A failure notice belongs to the scope and connection that observed it.
func (m Model) cronNoticeReset() Model {
	m.nextCronNotice++
	m.cronNotices = cronNoticeState{conn: m.connGen, scope: m.scopeGen, gen: m.nextCronNotice}
	if m.toast != nil && m.toast.cronFailure {
		m.toast = nil
	}
	return m
}
func (m Model) cronNoticeSuppressed(r cronRunEvent) bool {
	if _, ok := m.cronNotices.announced[r.UID]; ok {
		return true
	}
	if r.JobUID != "" {
		for _, old := range m.cronNotices.announced {
			if old.ProjectUID == r.ProjectUID && old.JobUID == r.JobUID && m.cronRunUnseen(old.ProjectUID, m.cronNoticeNewest(old)) {
				return true
			}
		}
	}
	return false
}

// The store folds a run's concurrent observations by an HLC the client never
// sees, so only a row read after the run's latest event, which settles its
// change mark, resolves it; an older in-flight read or a bare event does not.
func (m Model) cronNoticeNewest(r cronRunEvent) generated.CronRun {
	if m.cron.data != nil && m.cron.data.events >= r.seq && m.cron.changed[r.UID].at == 0 {
		if known, ok := m.cron.data.fetched(r.UID); ok {
			return known
		}
	}
	return r.run()
}

// fetched returns uid's row only when this refresh itself read it: from the
// head, an exact UID read, or a last-run fill. Retained page rows, rows kept
// after a failed read, and snapshot copies may predate the events it captured.
func (p cronData) fetched(uid string) (generated.CronRun, bool) {
	var rows []generated.CronRun
	if p.runsErr == nil {
		rows = slices.Clone(p.runs)
	}
	if err, read := p.runErr[uid]; read && err == nil {
		rows = append(rows, p.lookup[uid])
	}
	for job, r := range p.lastRuns {
		if err, read := p.lastErr[job]; read && err == nil {
			rows = append(rows, r)
		}
	}
	rows = cronMergeRuns(nil, rows)
	i := slices.IndexFunc(rows, func(r generated.CronRun) bool { return r.UID == uid })
	if i < 0 {
		return generated.CronRun{}, false
	}
	return rows[i], true
}
func (m Model) cronNoticeEvent(msg eventReceivedMsg) (Model, tea.Cmd) {
	m = m.cronNoticeSync()
	if msg.eventType != "cron.run.observed" || msg.cronRun == nil || m.scope.allProjects || m.scope.projectID == 0 || msg.projectID != m.scope.projectID {
		return m, nil
	}
	r := *msg.cronRun
	r.seq = m.cron.events
	if r.UID == "" || r.ProjectUID == "" || r.ProjectUID != msg.projectUID {
		return m, nil
	}
	if selected := m.cronProjectUID(); selected != "" && selected != r.ProjectUID {
		return m, nil
	}
	if r.Status != "failed" && r.Status != "unknown" {
		return m, nil // a correction may lose the fold; its read settles it
	}
	at := cronFailureTime(r.run())
	if at.IsZero() || at.Before(m.cronStartedAt.Add(-2*time.Minute)) {
		return m, nil
	}
	if m.cronSeen.ready && (!m.cronRunUnseen(r.ProjectUID, r.run()) || m.cronNoticeSuppressed(r)) {
		return m, nil
	}
	m.cronNotices.pending = maps.Clone(m.cronNotices.pending)
	if m.cronNotices.pending == nil {
		m.cronNotices.pending = map[string]cronRunEvent{}
	}
	m.cronNotices.pending[r.UID] = r
	return m.cronNoticeArm()
}
func (m Model) cronNoticeArm() (Model, tea.Cmd) {
	if !m.cronSeen.ready || m.cronNotices.window || len(m.cronNotices.pending) == 0 {
		return m, nil
	}
	m.cronNotices.pending = maps.Clone(m.cronNotices.pending)
	for uid, r := range m.cronNotices.pending {
		if !m.cronRunUnseen(r.ProjectUID, m.cronNoticeNewest(r)) || m.cronNoticeSuppressed(r) {
			delete(m.cronNotices.pending, uid)
		}
	}
	if len(m.cronNotices.pending) == 0 {
		return m, nil
	}
	m.nextCronNotice++
	m.cronNotices.gen = m.nextCronNotice
	m.cronNotices.window = true
	msg := cronNoticeTickMsg{conn: m.connGen, scope: m.scopeGen, gen: m.cronNotices.gen}
	return m, tea.Tick(cronNoticeWindow, func(time.Time) tea.Msg { return msg })
}
func (m Model) cronNoticeFlush(msg cronNoticeTickMsg) (Model, tea.Cmd) {
	if msg.conn != m.connGen || msg.scope != m.scopeGen || msg.gen != m.cronNotices.gen || !m.cronNotices.window {
		return m, nil
	}
	pending := m.cronNotices.pending
	m.cronNotices.pending = nil
	m.cronNotices.window = false
	if m.scope.allProjects || m.scope.projectID == 0 {
		return m, nil
	}
	eligible := []cronRunEvent{}
	for _, r := range pending {
		if m.cronRunUnseen(r.ProjectUID, m.cronNoticeNewest(r)) && !m.cronNoticeSuppressed(r) {
			eligible = append(eligible, r)
		}
	}
	if len(eligible) == 0 {
		return m, nil
	}
	m.cronNotices.announced = maps.Clone(m.cronNotices.announced)
	if m.cronNotices.announced == nil {
		m.cronNotices.announced = map[string]cronRunEvent{}
	}
	labels := map[string]bool{}
	for _, r := range eligible {
		m.cronNotices.announced[r.UID] = r
		label := "run " + truncate(r.UID, 12)
		if r.JobUID != "" {
			label = "job " + truncate(r.JobUID, 12)
			if m.cron.data != nil {
				if j := m.cron.data.job(r.JobUID); j != nil {
					label = j.Name
				}
			}
		}
		labels[sanitizeForLine(label)] = true
	}
	names := make([]string, 0, len(labels))
	for label := range labels {
		names = append(names, label)
	}
	slices.Sort(names)
	text := strings.Join(names[:min(2, len(names))], ", ")
	if len(names) > 2 {
		text += fmt.Sprintf(" +%d", len(names)-2)
	}
	text = truncate(text, max(m.width-52, 12))
	m.toast = &toast{text: fmt.Sprintf("cron: %s failed: %s", cronCount(len(eligible), "run"), text), level: toastError, expiresAt: m.toastNow().Add(10 * time.Second), cronFailure: true}
	return m, toastExpireCmd(10 * time.Second)
}

// A toast may survive navigation; render the hint for the keys active now.
func (m Model) cronToast() *toast {
	if m.toast == nil || !m.toast.cronFailure {
		return m.toast
	}
	out := *m.toast
	hint := "open Cron Runs to review"
	if m.canQuit() {
		switch m.view {
		case viewCron:
			if !m.cron.help {
				hint = "u to review"
			}
		case viewList, viewDetail:
			hint = "3 then u to review"
		}
	}
	out.text += " · " + hint
	return &out
}

// Below other views a failure notice keeps the error severity of the Cron row.
func (m Model) cronToastLine() string {
	t := m.cronToast()
	switch {
	case t == nil || !t.cronFailure:
		return renderToast(t)
	case m.cronStatusNotice() != nil:
		return ""
	}
	return errorStyle.Render(t.text)
}

// The Cron list shows its failure notice in the error row above help, so the
// shared toast line below the screen skips it there.
func (m Model) cronStatusNotice() *toast {
	if t := m.cronToast(); t != nil && t.cronFailure && m.view == viewCron && !m.cron.help && m.cron.form == nil &&
		m.width >= 80 && !m.scope.allProjects && m.scope.projectID != 0 {
		return t
	}
	return nil
}

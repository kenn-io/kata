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
	UID        string     `json:"uid"`
	JobUID     string     `json:"job_uid"`
	ProjectUID string     `json:"project_uid"`
	Status     string     `json:"status"`
	EndedAt    *time.Time `json:"ended_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (r cronRunEvent) run() generated.CronRun {
	return generated.CronRun{UID: r.UID, Status: r.Status, EndedAt: r.EndedAt, UpdatedAt: r.UpdatedAt}
}

type cronNoticeState struct {
	conn, scope, gen   uint64
	window             bool
	pending, announced map[string]cronRunEvent
}
type cronNoticeTickMsg struct{ conn, scope, gen uint64 }

func (m Model) cronNoticeSync() Model {
	if m.cronNotices.conn != m.connGen || m.cronNotices.scope != m.scopeGen {
		m.nextCronNotice++
		m.cronNotices = cronNoticeState{conn: m.connGen, scope: m.scopeGen, gen: m.nextCronNotice}
	}
	return m
}
func (m Model) cronNoticeSuppressed(r cronRunEvent) bool {
	if _, ok := m.cronNotices.announced[r.UID]; ok {
		return true
	}
	if r.JobUID != "" {
		for _, old := range m.cronNotices.announced {
			if old.ProjectUID == r.ProjectUID && old.JobUID == r.JobUID && m.cronRunUnseen(old.ProjectUID, old.run()) {
				return true
			}
		}
	}
	return false
}
func (m Model) cronNoticeEvent(msg eventReceivedMsg) (Model, tea.Cmd) {
	m = m.cronNoticeSync()
	if msg.eventType != "cron.run.observed" || msg.cronRun == nil || m.scope.allProjects || m.scope.projectID == 0 || msg.projectID != m.scope.projectID {
		return m, nil
	}
	r := *msg.cronRun
	if r.UID == "" || r.ProjectUID == "" || r.ProjectUID != msg.projectUID {
		return m, nil
	}
	if selected := m.cronProjectUID(); selected != "" && selected != r.ProjectUID {
		return m, nil
	}
	if r.Status != "failed" && r.Status != "unknown" {
		m.cronNotices.pending = maps.Clone(m.cronNotices.pending)
		m.cronNotices.announced = maps.Clone(m.cronNotices.announced)
		delete(m.cronNotices.pending, r.UID)
		delete(m.cronNotices.announced, r.UID)
		return m, nil
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
		if m.cronRunUnseen(r.ProjectUID, r.run()) && !m.cronNoticeSuppressed(r) {
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
				for _, j := range m.cron.data.jobs {
					if j.UID == r.JobUID {
						label = j.Name
						break
					}
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

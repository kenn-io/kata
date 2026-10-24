package cron

import (
	"encoding/base64"
	"encoding/json/v2"
	"maps"
	"strings"

	tea "charm.land/bubbletea/v2"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
)

type viewKind string

const (
	viewList     viewKind = "list"
	viewJob      viewKind = "job"
	viewRun      viewKind = "run"
	viewWorkflow viewKind = "workflow"
)

type navFrame struct {
	view                          viewKind
	tab                           int
	projectUID, uid, selectionKey string
	name                          string
	selectionOrdinal, scroll      int
	follow                        bool
	query                         string
	expanded                      map[string]bool
}

func detailItemKey(kind, projectUID, uid string) string {
	return kind + ":" + base64.RawURLEncoding.EncodeToString([]byte(projectUID)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(uid))
}
func parseDetailItemKey(key string) (kind, projectUID, uid string, ok bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 3 {
		return
	}
	kind = parts[0]
	switch kind {
	case "run", "step", "used-by", "job":
	default:
		return
	}
	p, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(p) != parts[1] {
		return
	}
	u, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(u) == 0 || base64.RawURLEncoding.EncodeToString(u) != parts[2] {
		return
	}
	return kind, string(p), string(u), true
}
func (m *model) currentView() viewKind {
	if len(m.nav) < 2 {
		return viewList
	}
	return m.nav[len(m.nav)-1].view
}
func (m *model) saveFrame() {
	if len(m.nav) == 0 {
		m.nav = []navFrame{{view: viewList}}
	}
	f := &m.nav[len(m.nav)-1]
	f.tab = m.tab
	f.scroll = m.scroll
	f.follow = m.follow
	f.query = m.query
	f.expanded = maps.Clone(m.expanded)
	if f.view == viewList {
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			f.selectionKey = m.rows[m.cursor].identity()
		}
		f.selectionOrdinal = m.cursor
	} else {
		f.selectionKey = m.detailKey
		f.selectionOrdinal = m.detailOrdinal
	}
}
func (m *model) pushDetail(kind viewKind, projectUID, uid string) tea.Cmd {
	m.saveFrame()
	for i, f := range m.nav {
		if f.view == kind && f.projectUID == projectUID && f.uid == uid {
			m.nav = m.nav[:i+1]
			m.restoreFrame(f)
			return m.pushEvent(kind, projectUID, uid)
		}
	}
	f := navFrame{view: kind, tab: m.tab, projectUID: projectUID, uid: uid, follow: kind != viewRun, expanded: map[string]bool{}}
	m.nav = append(m.nav, f)
	if len(m.nav) > 8 {
		m.nav = append(m.nav[:1], m.nav[2:]...)
	}
	m.restoreFrame(f)
	if kind == viewJob && m.detailKey == "" {
		if v := m.selectedJobView(); v != nil && len(v.runIndexes) == 0 {
			m.notify(statusInfo, "no loaded run observations")
		}
	}
	return m.pushEvent(kind, projectUID, uid)
}
func (m *model) popDetail() {
	if len(m.nav) <= 1 {
		return
	}
	m.nav = m.nav[:len(m.nav)-1]
	m.restoreFrame(m.nav[len(m.nav)-1])
}
func (m *model) restoreFrame(f navFrame) {

	m.clearStatus()
	if r, ok := m.resolveFrame(f); ok {
		m.nav[len(m.nav)-1].name = r.label
	}
	m.tab = f.tab
	m.query = f.query
	m.expanded = maps.Clone(f.expanded)
	if m.expanded == nil {
		m.expanded = map[string]bool{}
	}
	m.scroll = f.scroll
	m.follow = f.follow
	m.detailKey = f.selectionKey
	m.detailOrdinal = f.selectionOrdinal
	if f.view == viewList {
		m.rows = nil
		m.cursor = 0
		m.rebuild()
		for i, r := range m.rows {
			if r.identity() == f.selectionKey {
				m.cursor = i
				break
			}
		}
	} else {
		m.reconcileDetailSelection(m.detailItems())
	}
}
func (m *model) resolveFrame(f navFrame) (row, bool) {
	found := row{}
	matches := 0
	switch f.view {
	case viewJob:
		for i, j := range m.data.Jobs {
			if j.UID == f.uid && j.DeletedAt == nil && effectiveProjectUID(m.data, j.ProjectUID) == f.projectUID {
				found = row{kind: "job", projectUID: f.projectUID, uid: j.UID, label: j.Name, job: i}
				matches++
			}
		}
	case viewRun:
		for i, r := range m.data.Runs {
			if r.UID == f.uid && r.ProjectKey == f.projectUID {
				found = row{kind: "run", projectUID: f.projectUID, uid: r.UID, label: m.runView(r).Job, run: i}
				matches++
			}
		}
	case viewWorkflow:
		for i, w := range m.data.Workflows {
			if w.UID == f.uid && w.DeletedAt == nil && effectiveProjectUID(m.data, w.ProjectUID) == f.projectUID {
				found = row{kind: "workflow", projectUID: f.projectUID, uid: w.UID, label: w.Name, wf: i}
				matches++
			}
		}
	}
	return found, matches == 1
}
func (m *model) detailSelectionPolicy(kind viewKind) missingPolicy {
	switch kind {
	case viewJob:
		return keepMissing
	case viewWorkflow:
		return nearestSurvivor
	default:
		return noSelection
	}
}
func (m *model) detailItemExists(key string) bool {
	kind, project, uid, ok := parseDetailItemKey(key)
	if !ok {
		return true
	}
	switch kind {
	case "run":
		_, ok = m.resolveFrame(navFrame{view: viewRun, projectUID: project, uid: uid})
		return ok
	case "job", "used-by":
		if kind == "used-by" && m.currentView() == viewWorkflow {
			v := m.selectedWorkflow()
			if v == nil || v.key.projectUID != project {
				return false
			}
			for _, j := range v.jobs {
				if j.key == scopedRowKey(project, uid) {
					return true
				}
			}
			return false
		}
		_, ok = m.resolveFrame(navFrame{view: viewJob, projectUID: project, uid: uid})
		return ok
	case "step":
		if m.currentView() != viewWorkflow {
			return false
		}
		f := m.nav[len(m.nav)-1]
		if f.projectUID != project {
			return false
		}
		r, ok := m.resolveFrame(f)
		if !ok {
			return false
		}
		var wf defs.WorkflowDef
		if json.Unmarshal(m.data.Workflows[r.wf].Definition, &wf) != nil {
			return false
		}
		for _, step := range wf.Steps {
			if step.Key == uid {
				return true
			}
		}
	}
	return false
}
func (m *model) detailItems() []rowHit {
	if m.currentView() == viewRun {
		return nil
	}
	d := m.buildDetail()
	s := d.Selection()
	m.detailKey, m.detailOrdinal = s.Key, s.Ordinal
	return d.Items()
}
func (m *model) reconcileDetailSelection(items []rowHit) {
	s := resolveSelection(detailSelection{Key: m.detailKey, Ordinal: m.detailOrdinal}, items, m.detailSelectionPolicy(m.currentView()), m.detailItemExists)
	m.detailKey, m.detailOrdinal = s.Key, s.Ordinal
}

// nestedRun resolves the retained identity against all current journals, including
// those beyond the display cap. Catalogue membership never authorizes an action.
func (m *model) nestedRun() (row, bool) {
	kind, project, uid, ok := parseDetailItemKey(m.detailKey)
	if !ok {
		m.notify(statusInfo, "invalid detail selection")
		return row{}, false
	}
	if kind != "run" {
		return row{}, false
	}
	r, ok := m.resolveFrame(navFrame{view: viewRun, projectUID: project, uid: uid})
	if !ok {
		m.notify(statusInfo, "selected run was removed")
		return row{}, false
	}
	parent, exists := m.selected()
	if !exists {
		return row{}, false
	}
	rr := m.data.Runs[r.run]
	f := m.nav[len(m.nav)-1]
	switch parent.kind {
	case "job":
		if records.Value(rr.JobUID) != parent.uid || project != f.projectUID {
			return row{}, false
		}
	case "workflow":
		if records.Value(rr.WorkflowUID) != parent.uid || project != f.projectUID {
			return row{}, false
		}
	default:
		return row{}, false
	}
	return r, true
}

func (m *model) buildDetail() *detail {
	kind := m.currentView()
	if kind == viewJob {
		return m.buildJobDetail()
	}
	if kind == viewWorkflow {
		return m.buildWorkflowDetail()
	}
	d := newDetail(m.width, 14, detailSelection{Key: m.detailKey, Ordinal: m.detailOrdinal}, m.detailSelectionPolicy(kind), m.detailItemExists)
	f := m.nav[len(m.nav)-1]
	if _, ok := m.resolveFrame(f); !ok {
		d.Title("Record removed", "", subtleStyle)
		d.KV("identity", f.uid)
	}
	return d
}

// navStack is owned by this view.
type navStack struct {
	nav           []navFrame
	detailKey     string
	detailOrdinal int
}

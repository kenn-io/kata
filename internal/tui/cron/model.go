package cron

import (
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/colormode"
	"go.kenn.io/kata/internal/tui/cron/records"
)

// Data is the source-shaped native catalog and loaded evidence snapshot.
type Data struct {
	Project   string
	Projects  map[string]Data
	Jobs      []records.Job
	Workflows []records.Workflow
	Runs      []records.Run
	Problems  []string
	MoreRuns  bool
}

// Options configures the native cron child and its captured service boundary.
type Options struct {
	Services map[string]Service
	Wrap     func(tea.Msg) tea.Msg

	Now      func() time.Time
	Project  string
	Mouse    bool
	ReadOnly bool
}
type model struct {
	navStack
	active bool
	tabNav map[int][]navFrame
	formState
	marqueeState
	listState
	data          Data
	opts          Options
	now           func() time.Time
	width, height int
	listOnly      bool
	expanded      map[string]bool
	jobs          *jobIndex
	jobMatches    int
	wfIndex       workflowIndex
	workflowWraps map[workflowWrapKey][]string
	runViews      map[string]runViewData
	notice        statusNotice
	hits          hits
}

// Model is the ported Bubble Tea child.
type Model = model

// New constructs a cron child without starting polling or execution.
func New(o Options) *Model { return newModel(o) }
func newModel(o Options) *model {
	if o.Now == nil {
		o.Now = time.Now
	}
	applyColorMode(colormode.Resolve(), true)
	return &model{active: true, opts: o, now: o.Now, width: 80, height: 21, expanded: map[string]bool{}, follow: true, nav: []navFrame{{view: viewList, follow: true, expanded: map[string]bool{}}}}
}
func (m *model) SetData(d Data) {
	m.data = d
	sort.SliceStable(m.data.Runs, func(i, j int) bool {
		a, b := m.data.Runs[i], m.data.Runs[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		if a.UID != b.UID {
			return a.UID > b.UID
		}
		return a.ProjectKey < b.ProjectKey
	})
	m.reproject()
	m.rebuild()
	if m.currentView() != viewList {
		m.reconcileDetailSelection(m.detailItems())
	}
}
func (m *model) Init() tea.Cmd { return nil }
func (m *model) Update(msg tea.Msg) (next tea.Model, cmd tea.Cmd) {
	defer func() { cmd = tea.Batch(cmd, m.updateMarquee()) }()

	m.expireStatus()
	if cmd, handled := m.handleFormMessage(msg); handled {
		return m, cmd
	}
	switch v := msg.(type) {
	case marqueeTickMsg:
		if m.marqueePending && v.generation == m.marqueeGeneration {
			m.marqueePending = false
			m.frame++
		}

	case tea.WindowSizeMsg:
		m.width, m.height = max(0, v.Width), max(0, v.Height)
		m.workflowWraps = nil
	case tea.BackgroundColorMsg:
		if activeColorMode == colormode.Auto {
			applyColorMode(colormode.Auto, v.IsDark())
			m.workflowWraps = nil
		}
	case tea.KeyPressMsg:
		return m, m.routeKey(v)
	case tea.PasteMsg:
		if m.searching {
			m.query += v.Content
			m.rebuild()
		}
	case tea.MouseMsg:
		if m.opts.Mouse {
			return m, m.mouse(v)
		}
	}
	return m, nil
}
func (m *model) SelectTab(n int) {
	n = max(0, min(2, n))
	if n == m.tab {
		return
	}
	m.saveFrame()
	if m.tabNav == nil {
		m.tabNav = map[int][]navFrame{}
	}
	m.tabNav[m.tab] = append([]navFrame(nil), m.nav...)
	nav := m.tabNav[n]
	if len(nav) == 0 {
		nav = []navFrame{{view: viewList, tab: n, follow: true}}
	}
	m.nav = append([]navFrame(nil), nav...)
	m.restoreFrame(m.nav[len(m.nav)-1])
}

// CanQuit includes the conflict panel's advertised quit action.
func (m *model) CanQuit() bool { return m.CanLeave() || m.conflict != nil }
func (m *model) CanLeave() bool {
	return !m.searching && m.editor == nil && len(m.projectChoices) == 0 && len(m.confirm) == 0 && m.conflict == nil
}
func (m *model) reproject() {
	m.rebuildJobIndex()
	m.wfIndex = buildWorkflowIndex(m.data, *m.jobs, m.now())
}
func (m *model) rebuild() {
	selected := ""
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		selected = m.rows[m.cursor].identity()
	}
	m.runViews = map[string]runViewData{}
	for _, r := range m.data.Runs {
		m.runViews[scopedRowKey(r.ProjectKey, r.UID)] = m.runView(r)
	}
	var rows []row
	switch m.tab {
	case 0:
		for _, v := range m.visibleJobs() {
			rows = append(rows, row{kind: "job", projectUID: v.key.projectUID, uid: v.key.uid, label: v.entry.Name, search: v.search, job: v.index})
		}
		m.jobMatches = len(rows)
	case 1:
		for i, r := range m.data.Runs {
			v := m.runViews[scopedRowKey(r.ProjectKey, r.UID)]
			rows = append(rows, row{kind: "run", projectUID: r.ProjectKey, uid: r.UID, label: v.Job, search: runSearch(v, m.projectName(r.ProjectKey), m.now()), run: i})
		}
	case 2:
		rows = m.workflowRows()
	}
	m.rows = nil
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.search), strings.ToLower(m.query)) {
			m.rows = append(m.rows, r)
		}
	}
	if selected != "" {
		m.cursor = 0
		for i, r := range m.rows {
			if r.identity() == selected {
				m.cursor = i
				break
			}
		}
	}
	m.cursor = max(0, min(m.cursor, len(m.rows)-1))
}

type row struct {
	kind, projectUID, uid, label, search string
	job, run, wf                         int
}
type listState struct {
	follow              bool
	tab, cursor, scroll int
	query               string
	searching           bool
	rows                []row
}

func (m *model) projectName(key string) string      { return m.data.Projects[key].Project }
func (m *model) multipleProjects() bool             { return len(m.data.Projects) > 1 }
func (m *model) showProjects() bool                 { return m.multipleProjects() }
func effectiveProjectUID(_ Data, key string) string { return key }
func scopedRowKey(project, uid string) string       { return project + "\x00" + uid }
func (r row) identity() string                      { return scopedRowKey(r.projectUID, r.uid) }
func (m *model) selectedJobView() *jobView {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	return m.indexedJob(r)
}
func (m *model) selectedJobRun() (records.Run, bool) {
	v := m.selectedJobView()
	if v == nil {
		return records.Run{}, false
	}
	if m.currentView() == viewJob && m.detailKey != "" {
		if r, ok := m.nestedRun(); ok {
			return m.data.Runs[r.run], true
		}
		return records.Run{}, false
	}
	if len(v.runIndexes) > 0 {
		return m.data.Runs[v.runIndexes[0]], true
	}
	return records.Run{}, false
}
func (m *model) refresh() tea.Cmd { return func() tea.Msg { return RefreshMsg{} } }

// RefreshMsg requests a native catalog refresh.
type RefreshMsg struct{}

// MoreRunsMsg requests the next loaded-history page.
type MoreRunsMsg struct{}

// DetailMsg requests evidence for a captured project and record.
type DetailMsg struct{ Kind, ProjectKey, UID string }

// ActiveDetail identifies the currently visible evidence query without changing navigation.
func (m *model) ActiveDetail() (DetailMsg, bool) {
	if m.currentView() == viewList {
		return DetailMsg{}, false
	}
	f := m.nav[len(m.nav)-1]
	return DetailMsg{Kind: string(f.view), ProjectKey: f.projectUID, UID: f.uid}, true
}

// IssueMsg opens a captured issue in the native Kata browser.
type IssueMsg struct{ ProjectKey, UID string }

// TabMsg selects the outer native tab.
type TabMsg int

func issueCmd(project, uid string) tea.Cmd { return func() tea.Msg { return IssueMsg{project, uid} } }
func tabCmd(n int) tea.Cmd                 { return func() tea.Msg { return TabMsg(n) } }
func (m *model) pushEvent(kind viewKind, key, uid string) tea.Cmd {
	return func() tea.Msg { return DetailMsg{string(kind), key, uid} }
}

func (m *model) move(n int) {
	if m.currentView() != viewList {
		items := m.detailItems()
		if len(items) > 0 {
			s := moveSelection(detailSelection{Key: m.detailKey, Ordinal: m.detailOrdinal}, items, n)
			m.detailKey, m.detailOrdinal = s.Key, s.Ordinal
			m.follow = true
		} else {
			m.scroll = max(0, m.scroll+n)
			m.follow = false
		}
		return
	}
	before := m.cursor / m.pageRows()
	m.cursor = max(0, min(m.cursor+n, len(m.rows)-1))
	m.follow = true
	if m.cursor/m.pageRows() != before {
		m.scroll = 0
	}
}

func (m *model) selected() (row, bool) {
	if m.currentView() != viewList {
		return m.resolveFrame(m.nav[len(m.nav)-1])
	}
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

func (m *model) open() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	if m.currentView() == viewRun {
		return m.openJobOfRun()
	}
	if m.currentView() == viewJob || m.currentView() == viewWorkflow {
		kind, project, uid, valid := parseDetailItemKey(m.detailKey)
		if !valid {
			m.notify(statusInfo, "invalid detail selection")
			return nil
		}
		switch kind {
		case "run":
			if _, ok := m.nestedRun(); ok {
				return m.pushDetail(viewRun, project, uid)
			}
		case "used-by", "job":
			if m.detailItemExists(m.detailKey) {
				return m.pushDetail(viewJob, project, uid)
			}
		}
		return nil
	}
	switch r.kind {
	case "job":
		return m.pushDetail(viewJob, r.projectUID, r.uid)
	case "run":
		return m.pushDetail(viewRun, r.projectUID, r.uid)
	case "workflow":
		return m.pushDetail(viewWorkflow, r.projectUID, r.uid)
	}
	return nil
}

func (m *model) Resize(width, height int) {
	if m.editor != nil {
		m.editor.Update(tea.WindowSizeMsg{Width: width, Height: height})
	}
	m.width, m.height = max(0, width), max(0, height)
	m.workflowWraps = nil
}

func (m *model) SetActive(active bool) {
	if m.active == active {
		return
	}
	m.active = active
	if !active {
		m.marqueePending = false
		m.marqueeGeneration++
	}
}

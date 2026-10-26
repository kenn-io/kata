package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/tui/splitlayout"
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
	jobs                []generated.CronJob
	workflows           []generated.CronWorkflow
	runs                []generated.CronRun
	errs                [3]error
}
type cronLoadedMsg cronData
type cronState struct {
	tab             int
	lists           [4]cronList
	filtering, help bool
	stack           []cronFrame
	data            *cronData
	events          uint64
	out             bool
	conn, scope     uint64
}

// Synchronize after every update and retain visible records during refresh.
func (m Model) cronSync() (Model, tea.Cmd) {
	if m.cron.conn != m.connGen || m.cron.scope != m.scopeGen {
		m.cron = cronState{tab: m.cron.tab, conn: m.connGen, scope: m.scopeGen}
	}
	if m.view != viewCron || m.scope.allProjects || m.scope.projectID == 0 || m.cron.out {
		return m, nil
	}
	if m.cron.data != nil && m.cron.data.events == m.cron.events {
		return m, nil
	}
	m.cron.out = true
	return m, m.fetchCron()
}
func (m Model) fetchCron() tea.Cmd {
	reader, id := m.api, m.scope.projectID
	data := cronData{conn: m.connGen, scope: m.scopeGen, events: m.cron.events}
	return func() tea.Msg {
		data.jobs, data.errs[0] = cronRead(reader.ListCronJobs, id)
		data.workflows, data.errs[1] = cronRead(reader.ListCronWorkflows, id)
		data.runs, data.errs[2] = cronRead(reader.ListCronRuns, id)
		return cronLoadedMsg(data)
	}
}
func cronRead[T any](read func(context.Context, int64) (T, error), id int64) (T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
	defer cancel()
	return read(ctx, id)
}
func (m Model) cronLoaded(msg cronLoadedMsg) Model {
	if msg.conn != m.connGen || msg.scope != m.scopeGen {
		return m
	}
	data := cronData(msg)
	if previous := m.cron.data; previous != nil {
		if data.errs[0] != nil {
			data.jobs = previous.jobs
		}
		if data.errs[1] != nil {
			data.workflows = previous.workflows
		}
		if data.errs[2] != nil {
			data.runs = previous.runs
		}
	}
	m.cron.out, m.cron.data = false, &data
	return m
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
		if tab == 0 {
			return m
		}
		m.prevView = m.view
		if m.layout == splitlayout.Split && m.focus == focusDetail {
			m.prevView = viewDetail
		}
		m.view = viewCron
	}
	if tab == 0 {
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
	case key == "enter" || key == "l":
		if len(lines) > 0 && lines[*cursor].target.uid != "" {
			m.cron.stack = append(m.cron.stack, cronFrame{ref: lines[*cursor].target})
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
	case key == "r":
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

func cronJobState(j generated.CronJob) string {
	if j.Definition.Enabled {
		return "enabled"
	}
	return "paused"
}
func cronMatches(text, query string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(query))
}

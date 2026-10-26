package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

// cronHost keeps the native issue model and its input ownership intact.
// Only this outer model accounts for the three rows occupied by the tab bar.
type cronHost struct {
	slots                    chan struct{}
	board                    *cronui.Model
	runRequests              map[cronRunKey]uint64
	detailRequests           map[int64]uint64
	detailPending            map[int64]cronDetailFlight
	runPending               map[cronRunKey]bool
	jobPages                 map[cronRunKey]records.RunPage
	jobPageEvidence          map[cronRunKey]uint64
	cronScope                cronScope
	epoch, request           uint64
	catalogs                 map[int64]cronCatalog
	loading, dirty, debounce bool
	cronProblem              string
	cronDenied               bool
	issueRequest             uint64
	issueGenerations         map[int64]uint64
	issueReturnProjects      bool
	tabGeneration            uint64
	background               *tea.BackgroundColorMsg
	peekKey                  cronPeekKey
	peekGeneration           uint64
	peekCancel               context.CancelFunc
	peekAuth                 AuthInfo
	peekClosed               bool

	native             Model
	tab, width, height int
}

func newCronHost(native Model) *cronHost {
	h := &cronHost{native: native, dirty: true, slots: make(chan struct{}, 4)}
	h.syncCronScope()
	h.updateCronBoard()
	return h
}
func (h *cronHost) Init() tea.Cmd { return wrapNativeInput(h.native.Init(), h.epoch, h.tabGeneration) }
func (h *cronHost) Update(msg tea.Msg) (next tea.Model, cmd tea.Cmd) {
	defer func() {
		h.resizeCronBoard()
		cmd = tea.Batch(cmd, h.syncCronPeek())
	}()
	switch v := msg.(type) {
	case cronPeekMsg:
		h.acceptCronPeek(v)
		return h, nil
	case cronNativeInputMsg:
		if v.epoch != h.epoch || v.tabGeneration != h.tabGeneration || h.tab != 0 {
			return h, nil
		}
		msg = v.input
	case cronChildMsg:
		if v.epoch != h.epoch {
			return h, nil
		}
		return h, h.updateCronChild(v.msg)

	case cronCatalogMsg:
		if v.epoch != h.epoch || v.request != h.request {
			return h, nil
		}
		h.acceptCronCatalog(v)
		h.updateCronBoard()
		var runs tea.Cmd
		if h.tab > 0 {
			runs = h.loadCronRuns("", 0, false)
		}
		return h, tea.Batch(h.loadCronCatalog(), runs, h.loadVisibleCronDetail(), h.wakeCron())
	case cronRunsMsg:
		h.acceptCronRuns(v)
		h.updateCronBoard()
		var runs tea.Cmd
		if h.tab > 0 {
			runs = h.loadCronRuns("", 0, false)
		}
		return h, tea.Batch(runs, h.loadVisibleCronDetail(), h.wakeCron())
	case cronui.RefreshMsg:
		return h, h.refreshCron()
	case cronui.MoreRunsMsg:
		return h, h.loadCronRuns("", 0, true)
	case cronui.DetailMsg:
		return h, h.cronDetail(v)
	case cronui.IssueMsg:
		return h, h.openCronIssue(v)
	case cronui.TabMsg:
		if !h.canSwitchCronTabs() {
			return h, nil
		}
		return h, h.selectCronTab(int(v))
	case cronIssueMsg:
		if v.epoch != h.epoch || v.request != h.issueRequest || v.generation != h.issueGenerations[v.project] || !h.cronProjectAccessible(v.project) || h.board == nil || !h.board.CanLeave() || h.native.modal != modalNone {
			return h, nil
		}
		if v.err != nil {
			h.cronProblem = "No issue permission: " + v.err.Error()
			if records.Status(v.err) != 403 || records.AuthRejected(v.err) {
				h.clearCronAccess(v.err)
				h.cronProblem = cronProblemText(v.err)
			}
			h.updateCronBoard()
			return h, nil
		}
		if v.detail == nil || v.detail.Issue == nil {
			return h, nil
		}
		h.issueReturnProjects = h.issueReturnProjects || h.native.view == viewProjects
		h.native = h.native.seedOpenDetail(*v.detail.Issue)
		h.native.detail.scopePID = v.project
		h.native.detail.allProjects = h.cronScope.all
		transition := h.selectCronTab(0)
		return h, tea.Batch(h.native.fetchOpenDetail(), transition)
	case cronRunDetailMsg:
		if v.epoch != h.epoch || v.request != h.detailRequests[v.project] {
			return h, nil
		}
		pending, pendingOK := h.detailPending[v.project]
		follow := pendingOK && pending.request == v.request && pending.again
		if pendingOK && pending.request == v.request {
			delete(h.detailPending, v.project)
		}
		c, ok := h.catalogs[v.project]
		if !ok {
			return h, nil
		}
		if v.err != nil {
			if records.AuthRejected(v.err) {
				h.clearCronAccess(v.err)
				h.updateCronBoard()
				return h, nil
			}
			if records.Status(v.err) == 403 {
				h.denyCronProject(v.project, cronProblemText(v.err))
				h.updateCronBoard()
				return h, nil
			}
			c.detailProblem = cronProblemText(v.err)
		} else {
			v.run = h.reconcileCronRun(v.project, v.run)
			c = h.catalogs[v.project]
			c.detailProblem = ""
			c.runs = mergeCronRuns(c.runs, []records.Run{v.run})
		}
		h.catalogs[v.project] = c
		h.updateCronBoard()
		if follow {
			detail, active := h.board.ActiveDetail()
			if active && detail.Kind == "run" && detail.ProjectKey == fmt.Sprint(v.project) && detail.UID == pending.uid {
				return h, h.cronDetail(detail)
			}
		}
		return h, nil

	case cronDebounceMsg:
		if v.epoch != h.epoch {
			return h, nil
		}
		h.debounce = false
		return h, h.loadCronCatalog()

	case tea.BackgroundColorMsg:
		h.background = &v
	case tea.WindowSizeMsg:
		h.width, h.height = v.Width, v.Height
		msg = tea.WindowSizeMsg{Width: v.Width, Height: max(0, v.Height-3)}
	case tea.KeyPressMsg:
		h.issueRequest++
		if h.native.modal != modalNone {
			next, cmd := h.native.routeModalKey(v)
			h.native = next
			return h, cmd
		}
		if h.tab > 0 && (v.String() == "ctrl+c" || v.String() == "q" && h.board.CanQuit()) {
			next, cmd := h.native.Update(v)
			h.native = next.(Model)
			return h, cmd
		}
		if h.canSwitchCronTabs() && len(v.String()) == 1 && v.String()[0] >= '1' && v.String()[0] <= '4' {
			return h, h.selectCronTab(int(v.String()[0] - '1'))
		}
	case tea.MouseMsg:
		switch v.(type) {
		case tea.MouseClickMsg, tea.MouseWheelMsg:
			h.issueRequest++
		}
		if h.native.modal != modalNone {
			return h, nil
		}
		if h.native.opts.Mouse && h.canSwitchCronTabs() && h.cronTabHeight() > 0 {
			if click, ok := v.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
				if tab, ok := h.board.TabAt(h.tab, h.width, click.X, click.Y); ok {
					return h, h.selectCronTab(tab)
				}
			}
		}
		if h.tab > 0 && h.width < 80 && h.board.CanLeave() {
			return h, nil
		}
		offset := h.cronTabHeight()
		msg = cronMouseOffset(v, offset)
		if msg == nil {
			return h, nil
		}
	}
	if h.tab > 0 {
		switch msg.(type) {
		case tea.KeyPressMsg, tea.PasteMsg, tea.MouseMsg:
			return h, h.updateCronChild(msg)
		}
	}
	_, back := msg.(popDetailMsg)
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "esc" && h.native.view == viewDetail && len(h.native.detail.navStack) == 0 {
		back = true
	}
	next, cmd = h.native.Update(msg)
	h.native = next.(Model)
	if h.issueReturnProjects && back && h.native.view == viewList {
		h.native.view = viewProjects
		h.issueReturnProjects = false
		cmd = nil
	} else if h.native.view == viewList {
		h.issueReturnProjects = false
	}
	h.syncCronScope()
	h.updateCronBoard()
	var invalidate tea.Cmd
	switch v := msg.(type) {
	case eventReceivedMsg:
		if !h.native.staleConnMsg(v.gen) {
			if strings.HasPrefix(v.eventType, "cron.") {
				invalidate = h.invalidateCron(v.projectUID)
			} else if strings.HasPrefix(v.eventType, "project.") {
				invalidate = h.invalidateCron("")
			}
		}
	case resetRequiredMsg:
		if !h.native.staleConnMsg(v.gen) {
			invalidate = h.invalidateCron("")
		}
	case sseStatusMsg:
		if !h.native.staleConnMsg(v.gen) && v.state == sseConnected {
			invalidate = h.invalidateCron("")
		}
	}
	return h, tea.Batch(wrapNativeInput(cmd, h.epoch, h.tabGeneration), invalidate, h.loadCronCatalog())
}

func cronMouseOffset(msg tea.MouseMsg, offset int) tea.Msg {
	p := msg.Mouse()
	if p.Y < offset {
		return nil
	}
	p.Y -= offset
	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(p)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(p)
	case tea.MouseWheelMsg:
		return tea.MouseWheelMsg(p)
	case tea.MouseMotionMsg:
		return tea.MouseMotionMsg(p)
	}
	return nil
}

func (h *cronHost) View() tea.View {
	v := h.native.View()
	if h.width <= 0 || h.height <= 0 {
		v.SetContent("")
		return v
	}
	body := v.Content
	if h.tab > 0 {
		cronBody := h.board.View().Content
		if h.width < 80 && h.board.CanLeave() {
			cronBody = renderTooNarrow(h.width, max(0, h.height-h.cronTabHeight()))
		}
		body = cronBody
		if h.native.modal != modalNone {
			body = overlayModal(body, renderConfirmModal(h.native.modal), h.width, max(0, h.height-h.cronTabHeight()))
		}
	}
	text := body
	if h.cronTabHeight() > 0 {
		text = h.board.Tabs(h.tab, h.width) + "\n" + body
	}

	lines := strings.Split(text, "\n")
	if h.height > 0 && len(lines) > h.height {
		lines = lines[:h.height]
	}
	for i := range lines {
		if h.width > 0 {
			lines[i] = ansi.Truncate(lines[i], h.width, "")
		}
	}
	v.SetContent(strings.Join(lines, "\n"))
	return v
}

func (h *cronHost) canSwitchCronTabs() bool {
	if h.native.modal != modalNone {
		return false
	}
	if h.tab == 0 {
		return h.native.canQuit() && !h.native.undoInFlight && !h.native.list.pendingPriority && (h.native.view != viewFederation || h.native.federation.mode != federationModeAdoptConfirm)
	}
	return h.board.CanLeave()
}

type cronTabIntentMsg struct {
	generation uint64
	tab        int
}

type cronIssueIntentMsg struct {
	request uint64
	issue   cronui.IssueMsg
}

type cronChildMsg struct {
	epoch uint64
	msg   tea.Msg
}

func (h *cronHost) wrapCronCmd(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	epoch, request, tabGeneration := h.epoch, h.issueRequest, h.tabGeneration
	return func() tea.Msg {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			out := make(tea.BatchMsg, len(batch))
			for i, c := range batch {
				out[i] = wrapCronEpoch(c, epoch, request, tabGeneration)
			}
			return out
		}
		switch msg.(type) {
		case tea.QuitMsg:
			return msg
		}
		switch intent := msg.(type) {
		case cronui.IssueMsg:
			msg = cronIssueIntentMsg{request: request, issue: intent}
		case cronui.TabMsg:
			msg = cronTabIntentMsg{generation: tabGeneration, tab: int(intent)}
		}
		return cronChildMsg{epoch, msg}
	}
}
func wrapCronEpoch(cmd tea.Cmd, epoch, request, tabGeneration uint64) tea.Cmd {
	h := &cronHost{epoch: epoch, issueRequest: request, tabGeneration: tabGeneration}
	return h.wrapCronCmd(cmd)
}
func (h *cronHost) updateCronChild(msg tea.Msg) tea.Cmd {
	switch v := msg.(type) {
	case cronIssueIntentMsg:
		if v.request != h.issueRequest {
			return nil
		}
		return h.openCronIssue(v.issue)
	case cronTabIntentMsg:
		if v.generation != h.tabGeneration || !h.canSwitchCronTabs() {
			return nil
		}
		return h.selectCronTab(v.tab)
	case cronui.ExecMsg:
		if h.board == nil || !h.board.ExecCurrent(v) {
			return nil
		}
		return v.Cmd
	case cronui.AccessLostMsg:
		err := &APIError{Status: v.Status, Code: v.Code, Message: v.Message}
		if records.AuthRejected(err) {
			h.clearCronAccess(err)
		} else if id, parseErr := strconv.ParseInt(v.ProjectKey, 10, 64); parseErr == nil {
			h.denyCronProject(id, cronProblemText(err))
		}
		h.updateCronBoard()
		return nil
	case cronui.ChangedMsg:
		if h.board == nil || !h.board.ChangeCurrent(v) {
			return nil
		}
		if v.Deleted != nil {
			j := *v.Deleted
			for id, c := range h.catalogs {
				if fmt.Sprint(id) == j.ProjectUID {
					c.jobs = slices.DeleteFunc(c.jobs, func(old records.Job) bool { return old.UID == j.UID })
					h.catalogs[id] = c
				}
			}
		}
		if v.SavedWorkflow != nil {
			w := *v.SavedWorkflow
			for id, c := range h.catalogs {
				if fmt.Sprint(id) != w.ProjectUID {
					continue
				}
				found := false
				for i, old := range c.workflows {
					if old.UID == w.UID {
						c.workflows[i] = w
						found = true
						break
					}
				}
				if !found {
					c.workflows = append(c.workflows, w)
				}
				h.catalogs[id] = c
			}
		}
		if v.Saved != nil {
			j := *v.Saved
			for id, c := range h.catalogs {
				if fmt.Sprint(id) == j.ProjectUID {
					found := false
					for i, old := range c.jobs {
						if old.UID == j.UID {
							j.LastRun, j.LastRunKnown = old.LastRun, old.LastRunKnown
							c.jobs[i] = j
							found = true
							break
						}
					}
					if !found {
						c.jobs = append(c.jobs, j)
					}
					h.catalogs[id] = c
				}
			}
		}
		h.request++
		h.loading = false
		h.dirty = true
		h.updateCronBoard()
		return h.loadCronCatalog()
	case cronui.TabMsg:
		if !h.canSwitchCronTabs() {
			return nil
		}
		return h.selectCronTab(int(v))
	case cronui.DetailMsg:
		return h.cronDetail(v)
	case cronui.IssueMsg:
		return h.openCronIssue(v)
	case cronui.MoreRunsMsg:
		return h.loadCronRuns("", 0, true)
	case cronui.RefreshMsg:
		return h.refreshCron()
	}
	_, cmd := h.board.Update(msg)
	return h.wrapCronCmd(cmd)
}

func (h *cronHost) wakeCron() tea.Cmd {
	if h.tab == 0 {
		return nil
	}
	return h.updateCronChild(struct{}{})
}

// Capture tab ownership before a deferred native form-opening command runs.
type cronNativeInputMsg struct {
	epoch, tabGeneration uint64
	input                openInputMsg
}

func wrapNativeInput(cmd tea.Cmd, epoch, generation uint64) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			wrapped := make(tea.BatchMsg, len(batch))
			for i, child := range batch {
				wrapped[i] = wrapNativeInput(child, epoch, generation)
			}
			return wrapped
		}
		if input, ok := msg.(openInputMsg); ok {
			return cronNativeInputMsg{epoch: epoch, tabGeneration: generation, input: input}
		}
		return msg
	}
}

func (h *cronHost) cronTabHeight() int {
	if h.tab == 0 || h.board.TabsVisible() {
		return 3
	}
	return 0
}
func (h *cronHost) resizeCronBoard() { h.board.Resize(h.width, max(0, h.height-h.cronTabHeight())) }

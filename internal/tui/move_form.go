package tui

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"go.kenn.io/kit/tui/termtext"
)

// moveAPI moves an issue and its descendants to another project, or previews
// the move. A client that cannot is refused with a message when M is pressed.
type moveAPI interface {
	MoveIssue(ctx context.Context, projectID int64, ref string, in MoveInput) (*MutationResp, error)
}

// moveChoice is one project the picker offers; label tells same-named
// projects apart.
type moveChoice struct {
	project ProjectSummary
	label   string
}

// movePreview is what the daemon said a move would do. revision is the one
// the daemon read, which the move is guarded on.
type movePreview struct {
	revision int64
	uids     []string

	descendants []string
}

// moveState is the Move form behind `M`: first a project picker, then the
// daemon's preview of moving there. It is a value on Model, replaced whole on
// every change, so a render never sees a half-updated form.
type moveState struct {
	target  formTarget
	formGen int64
	// mover is the connection's move client, checked once when the form opens.
	mover moveAPI
	// ownProjectUID names the issue's project (target.projectID is its id),
	// which is never a destination.
	ownProjectUID string

	// seq numbers the request whose answer the form waits for; an answer to
	// an older one is dropped.
	seq     int
	sending bool
	loading bool

	filter   textinput.Model
	projects []ProjectSummary
	cursor   int
	// nameContext maps a project UID to the area or UID shown beside a
	// name another project shares.
	nameContext map[string]string
	// projectNames maps every project id to its name, for naming where a
	// previewed descendant lives.
	projectNames map[int64]string

	chosen  ProjectSummary
	preview *movePreview
	err     string
	note    string
}

// moveStep is what the Move form is doing, which decides what enter does.
type moveStep int

const (
	movePicking moveStep = iota
	movePreviewing
	moveSending
)

func (s moveState) step() moveStep {
	switch {
	case s.sending:
		return moveSending
	case s.preview != nil:
		return movePreviewing
	}
	return movePicking
}

// closeMoveForm drops an open Move form, as when the daemon's answer about
// what this principal may write takes the permission away.
func (m Model) closeMoveForm() Model {
	if m.modal == modalMoveIssue {
		m.move, m.modal = moveState{}, modalNone
	}
	return m
}

// moveProjectsMsg carries the projects the picker offers.
type moveProjectsMsg struct {
	connGen  uint64
	formGen  int64
	projects []ProjectSummary
	err      error
}

// movePreviewMsg is the daemon's answer to a dry run.
type movePreviewMsg struct {
	connGen uint64
	formGen int64
	seq     int
	resp    *MutationResp
	err     error
}

// M is offered only on the selected row of the plain issue list. The detail
// view, the Inbox and a detail opened from Attention keep saved copies of an
// issue (Back history, the split pane, the return context) that a move would
// leave stale, so the key is not offered there. moveHidden decides the footer
// and help, moveTarget the dispatch.

// moveHidden reports whether credentials, a saved browsing context, or a
// pending undo or reopen lookup keep M off the footer and help. The lookup owns
// the issue until it answers, and a reopen lookup that answers opens its own
// confirmation over an open Move form.
func (m Model) moveHidden() bool {
	return !m.mutationAllowed(false) || m.moveBlockedByContext() || m.moveWaiting()
}

func (m Model) moveBlockedByContext() bool {
	return m.inboxReturn != nil || m.attention.returnContext != nil
}

func (m Model) moveWaiting() bool { return m.undoInFlight || m.reopenLookupPending }

// moveTarget is the issue M acts on: the selected row of the plain issue list,
// and nothing in any other view, pane or context.
func (m Model) moveTarget() (formTarget, Issue, bool) {
	if (m.view != viewList && m.view != viewDetail) || !m.listIsActive() || m.moveBlockedByContext() || m.moveWaiting() {
		return formTarget{}, Issue{}, false
	}
	return m.activeIssueTarget()
}

func (m Model) routeMoveKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.MoveIssue.matches(msg) {
		return m, nil, false
	}
	target, issue, ok := m.moveTarget()
	if !ok {
		return m, nil, true
	}
	mover, ok := m.api.(moveAPI)
	if !ok {
		next, cmd := m.undoNotice("this connection cannot move issues", toastError)
		return next, cmd, true
	}
	// Like any key after `!`, this one cancels the priority prompt, which
	// names the issue the selection is about to leave.
	m.list = m.list.cancelPriorityPrompt()
	target.issueUID = issue.UID
	m.nextFormGen++
	filter := textinput.New()
	filter.Prompt = ""
	_ = filter.Focus()
	m.move = moveState{
		target: target, formGen: m.nextFormGen, loading: true, filter: filter,
		mover: mover, ownProjectUID: issue.ProjectUID,
	}
	m.modal = modalMoveIssue
	return m, m.fetchMoveProjects(), true
}

func (m Model) fetchMoveProjects() tea.Cmd {
	apiClient, connGen, formGen := m.api, m.connGen, m.move.formGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		projects, err := apiClient.ListProjects(ctx)
		return moveProjectsMsg{connGen: connGen, formGen: formGen, projects: projects, err: err}
	}
}

func (m Model) handleMoveProjects(msg moveProjectsMsg) Model {
	if m.staleConnMsg(msg.connGen) || m.modal != modalMoveIssue || msg.formGen != m.move.formGen {
		return m
	}
	s := m.move
	s.loading = false
	if msg.err != nil {
		s.err = "projects unavailable: " + sanitizeForDisplay(msg.err.Error())
		m.move = s
		return m
	}
	s.projectNames = map[int64]string{}
	s.projects = nil
	for _, p := range msg.projects {
		s.projectNames[p.ID] = p.Name
		own := p.ID == s.target.projectID || (s.ownProjectUID != "" && p.UID == s.ownProjectUID)
		if own || p.UID == "" || isInboxProject(p) {
			continue
		}
		s.projects = append(s.projects, p)
	}
	slices.SortFunc(s.projects, func(a, b ProjectSummary) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.UID, b.UID)
	})
	s.nameContext = moveDestinationContexts(s.projects)
	m.move = s
	return m
}

// moveDestinationContexts names what tells same-named projects apart, as the
// web's move dialog does: the project's area when no other project of that
// name shares it, otherwise its UID. A project with a unique name needs none.
func moveDestinationContexts(projects []ProjectSummary) map[string]string {
	groups := map[string][]ProjectSummary{}
	for _, p := range projects {
		key := strings.ToLower(strings.TrimSpace(p.Name))
		groups[key] = append(groups[key], p)
	}
	nameContext := map[string]string{}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		areaCount := map[string]int{}
		for _, p := range group {
			if area := strings.TrimSpace(jsonString(p.Metadata.Area)); area != "" {
				areaCount[area]++
			}
		}
		for _, p := range group {
			if area := strings.TrimSpace(jsonString(p.Metadata.Area)); area != "" && areaCount[area] == 1 {
				nameContext[p.UID] = area
			} else {
				nameContext[p.UID] = p.UID
			}
		}
	}
	return nameContext
}

// choices are the projects whose name or context contains the filter text,
// in any case.
func (s moveState) choices() []moveChoice {
	query := strings.ToLower(strings.TrimSpace(s.filter.Value()))
	var out []moveChoice
	for _, p := range s.projects {
		extra := s.nameContext[p.UID]
		if query != "" && !strings.Contains(strings.ToLower(p.Name), query) &&
			!strings.Contains(strings.ToLower(extra), query) {
			continue
		}
		label := p.Name
		if extra != "" {
			label += " · " + extra
		}
		out = append(out, moveChoice{project: p, label: label})
	}
	return out
}

// routeMoveModalKey delivers a key to the open Move form.
func (m Model) routeMoveModalKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	s := m.move
	key := msg.String()
	if s.sending {
		return m, nil
	}
	if s.preview != nil {
		switch key {
		case "esc":
			s.preview, s.note, s.err = nil, "", ""
			m.move = s
		case "enter":
			return m.dispatchMove()
		}
		return m, nil
	}
	switch key {
	case "esc":
		return m.closeMoveForm(), nil
	case "up":
		s.cursor = max(s.cursor-1, 0)
		m.move = s
	case "down":
		s.cursor = min(s.cursor+1, max(len(s.choices())-1, 0))
		m.move = s
	case "ctrl+u":
		s.filter.SetValue("")
		s.cursor, s.err = 0, ""
		m.move = s
	case "enter":
		choices := s.choices()
		if s.loading || len(choices) == 0 {
			return m, nil
		}
		s.chosen = choices[min(s.cursor, len(choices)-1)].project
		m.move = s
		return m.dispatchMovePreview()
	default:
		m.move = s.filterUpdated(msg, m.width, m.height)
	}
	return m, nil
}

// filterUpdated feeds a key or paste to the filter and returns to the first
// match when the text changed.
func (s moveState) filterUpdated(msg tea.Msg, width, height int) moveState {
	s.filter.SetWidth(moveFilterWidth(width, height))
	before := s.filter.Value()
	s.filter, _ = s.filter.Update(msg)
	if s.filter.Value() != before {
		s.cursor, s.err = 0, ""
	}
	return s
}

// pasteIntoMoveFilter puts pasted text into the picker's filter.
func (m Model) pasteIntoMoveFilter(msg tea.PasteMsg) Model {
	if m.move.sending || m.move.preview != nil {
		return m
	}
	m.move = m.move.filterUpdated(msg, m.width, m.height)
	return m
}

// dispatchMovePreview asks the daemon what moving to the chosen project would
// do. It sends no If-Match, so the preview reads the issue as it stands now.
func (m Model) dispatchMovePreview() (Model, tea.Cmd) {
	s := m.move
	s.seq++
	s.sending, s.err = true, ""
	m.move = s
	in := MoveInput{Actor: m.list.actor, ToProjectUID: s.chosen.UID, DryRun: true}
	connGen, formGen, seq := m.connGen, s.formGen, s.seq
	request := m.moveRequest(in)
	return m, func() tea.Msg {
		resp, err := request()
		return movePreviewMsg{connGen: connGen, formGen: formGen, seq: seq, resp: resp, err: err}
	}
}

// moveRequest is the daemon call for the open form's issue, shared by the
// preview and the move itself.
func (m Model) moveRequest(in MoveInput) func() (*MutationResp, error) {
	mover, target := m.move.mover, m.move.target
	return func() (*MutationResp, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return mover.MoveIssue(ctx, target.projectID, target.issueShortID, in)
	}
}

func (m Model) handleMovePreview(msg movePreviewMsg) Model {
	if m.staleConnMsg(msg.connGen) || m.modal != modalMoveIssue ||
		msg.formGen != m.move.formGen || msg.seq != m.move.seq {
		return m
	}
	s := m.move
	s.sending = false
	if msg.err != nil {
		s.preview = nil
		s.err = sanitizeForDisplay(msg.err.Error())
		m.move = s
		return m
	}
	if msg.resp == nil {
		s.err = "the daemon's preview did not describe the move"
		m.move = s
		return m
	}
	preview := &movePreview{revision: msg.resp.Revision}
	for _, issue := range msg.resp.Issues {
		preview.uids = append(preview.uids, issue.UID)
		if issue.UID == s.target.issueUID {
			continue
		}
		ref := "#" + issue.ShortID
		if name := s.projectNames[issue.ProjectID]; name != "" {
			ref = name + ref
		}
		preview.descendants = append(preview.descendants, sanitizeForLine(ref+" "+issue.Title))
	}
	s.preview = preview
	m.move = s
	return m
}

// dispatchMove performs the move the preview showed, guarded on the revision
// the preview read.
func (m Model) dispatchMove() (Model, tea.Cmd) {
	s := m.move
	if s.preview.revision <= 0 {
		s.err = "The preview is incomplete, so the move was not sent. Press Esc and choose the project again."
		m.move = s
		return m, nil
	}
	s.sending, s.err, s.note = true, "", ""
	m.move = s
	in := MoveInput{Actor: m.list.actor, ToProjectUID: s.chosen.UID, IfMatchRevision: s.preview.revision}
	formGen := s.formGen
	request := m.moveRequest(in)
	return m, withConnGen(func() tea.Msg {
		resp, err := request()
		return mutationDoneMsg{origin: "move", kind: "move", formGen: formGen, resp: resp, err: err}
	}, m.connGen)
}

// handleMoveDone applies the answer to a move. A stale revision previews again
// at the current one; any other refusal stays on the preview to be read.
func (m Model) handleMoveDone(mut mutationDoneMsg) (Model, tea.Cmd) {
	if m.modal != modalMoveIssue || mut.formGen != m.move.formGen {
		return m, nil
	}
	s := m.move
	switch {
	case isRevisionConflict(mut.err):
		s.preview = nil
		s.note = "The issue's revision changed since the preview; nothing was moved. Showing the current preview."
		m.move = s
		return m.dispatchMovePreview()
	case mut.err != nil:
		s.sending = false
		s.err = "move failed: " + sanitizeForDisplay(mut.err.Error())
		m.move = s
		return m, nil
	}
	return m.finishMove(mut)
}

// finishMove closes the form after a move. The issues the daemon reports moved
// (the preview's list when the reply carries none) leave the list as deleted
// ones do, and the notice names the new project#id. A child added after the
// preview is in the reply, so it goes too.
func (m Model) finishMove(mut mutationDoneMsg) (Model, tea.Cmd) {
	s := m.move
	uid := s.target.issueUID
	gone := []string{uid}
	reported := s.preview.uids
	if mut.resp != nil && len(mut.resp.Issues) > 0 {
		reported = reported[:0:0]
		for _, issue := range mut.resp.Issues {
			reported = append(reported, issue.UID)
		}
	}
	for _, moved := range reported {
		if !slices.Contains(gone, moved) {
			gone = append(gone, moved)
		}
	}
	newRef := s.chosen.Name
	if mut.resp != nil && mut.resp.NewShortID != "" {
		newRef += "#" + mut.resp.NewShortID
	}
	m = m.closeMoveForm()
	m, cmd := m.dropMovedIssues(uid, gone)
	// Labels travel with the issues, from projects the preview may not have
	// named, so every cached project's counts are stale.
	var cached []int64
	if m.projectLabels != nil {
		cached = slices.Collect(maps.Keys(m.projectLabels.byProject))
	}
	m, labels := m.refreshCachedLabels(cached...)
	m, notice := m.undoNotice(sanitizeForLine(fmt.Sprintf("moved #%s to %s", s.target.issueShortID, newRef)), toastSuccess)
	return m, combineCmds(cmd, combineCmds(labels, notice))
}

// dropMovedIssues removes the moved issues from the list the move was started
// from: the rows leave the list and the selection goes to the row after anchor
// (the one before it after the last), a detail pane that shows one of them
// resets, the cached lists go stale, and the list is fetched again. A pane on
// an unrelated issue stays as it is, even when it no longer shows the
// highlighted row: the list edit does not make it follow the new selection.
func (m Model) dropMovedIssues(anchor string, gone []string) (Model, tea.Cmd) {
	hadIssue := m.detail.issue != nil
	m.detail = m.detail.withoutIssues(gone...)
	drop := func(lm listModel) listModel { return lm.droppingIssues(anchor, gone) }
	var follow, detailCmd tea.Cmd
	if hadIssue && m.detail.issue != nil {
		m = m.editListDay(drop)
	} else {
		m, follow = m.refreshListDay(drop)
		m, detailCmd = m.maybeBootstrapSplitDetail()
	}
	if m.cache != nil {
		m.cache.markStale()
	}
	return m, combineCmds(follow, combineCmds(detailCmd, withConnGen(m.list.refetchCmd(m.api, m.scope), m.connGen)))
}

// withoutIssues resets a detail pane that shows a moved issue, as itself, a
// child, or a Back entry, or that shows no issue yet. A pane inspecting an
// unrelated issue keeps it and its Back history.
func (dm detailModel) withoutIssues(gone ...string) detailModel {
	shows := func(issue *Issue) bool { return issue != nil && slices.Contains(gone, issue.UID) }
	if dm.issue != nil && !shows(dm.issue) &&
		!slices.ContainsFunc(dm.children, func(child Issue) bool { return shows(&child) }) &&
		!slices.ContainsFunc(dm.navStack, func(prev detailModel) bool { return shows(prev.issue) }) {
		return dm
	}
	dm.cancelReads()
	return newDetailModel()
}

// moveInnerWidth is the text width of the Move form: the box's interior, or the
// whole row when the terminal is too short for a border.
func moveInnerWidth(width, height int) int {
	if height < formMinHeight {
		return max(width, 1)
	}
	return max(min(width*7/10, 80), min(width-8, 30))
}

// moveFilterWidth bounds the picker's filter to its line, which also holds the
// "Project: " label and the cursor. It is set before each key as well as on
// render, because the input scrolls its text when it is typed into.
func moveFilterWidth(width, height int) int {
	return max(moveInnerWidth(width, height)-len("Project: ")-1, 1)
}

// renderMoveModal draws the project picker, or the daemon's preview once a
// project is chosen. Like the Delete form it always fits the terminal: below
// formMinHeight it drops the border and padding, and the rows that can be cut
// (descendants, the picker's list, an explanatory sentence) give way before
// the title, the outcome, a refusal and the enter/esc prompt do.
func renderMoveModal(s moveState, width, height int) string {
	boxed := height >= formMinHeight
	// The footer states what the keys do wherever it is drawn; without one the
	// modal says it itself.
	footerShown := footerDrawn(width, height)
	innerW := moveInnerWidth(width, height)
	budget := height
	if footerShown {
		budget-- // the footer keeps its row
	}
	if boxed {
		budget -= 4 // border and padding
	}
	wrap := func(text string) []string { return termtext.Wrap(text, max(innerW, 1)) }
	styled := func(lines []string) []string {
		out := make([]string, len(lines))
		for i, line := range lines {
			out[i] = titleStyle.Render(line)
		}
		return out
	}
	oneLine := func(text string) []string { return []string{truncate(text, max(innerW, 1))} }

	// What must stay: the refusal, the conflict note and the action prompt.
	// The title and the outcome shrink first, then the optional sentence and
	// the list.
	var tail []string
	if s.note != "" {
		tail = append(tail, wrap(s.note)...)
	}
	if s.err != "" {
		tail = append(tail, strings.Split(errorStyle.Width(max(innerW, 1)).Render(s.err), "\n")...)
	}
	previewing := s.preview != nil || (s.sending && s.chosen.UID != "")
	switch {
	case s.sending:
		tail = append(tail, statusStyle.Render("working…"))
	case !footerShown && previewing:
		tail = append(tail, subtleStyle.Render("enter move · esc back"))
	case !footerShown:
		tail = append(tail, subtleStyle.Render("enter preview · esc cancel"))
	}

	var titleText string
	var outcome, optional []string
	var extra []string // lines that belong to the step itself (the picker's filter)
	var list func(room int) []string
	if previewing {
		titleText = sanitizeForLine(fmt.Sprintf("Move #%s to %s", s.target.issueShortID, s.chosen.Name))
		if preview := s.preview; preview != nil {
			if len(preview.descendants) == 0 {
				outcome = []string{"This issue has no descendants, so it moves alone."}
			} else {
				noun := "descendants"
				if len(preview.descendants) == 1 {
					noun = "descendant"
				}
				outcome = []string{fmt.Sprintf("This issue and its %d %s move together.", len(preview.descendants), noun)}
				list = func(room int) []string {
					shown := len(preview.descendants)
					if shown > room {
						shown = max(room-1, 0)
					}
					var rows []string
					for _, line := range preview.descendants[:shown] {
						rows = append(rows, "  "+truncate(line, max(innerW-2, 1)))
					}
					if more := len(preview.descendants) - shown; more > 0 && room > 0 {
						rows = append(rows, subtleStyle.Render(fmt.Sprintf("  … and %d more", more)))
					}
					return rows
				}
			}
			if s.note == "" { // a conflict note already says nothing moved
				optional = wrap("Nothing has moved yet.")
			}
		}
	} else {
		s.filter.SetWidth(moveFilterWidth(width, height))
		titleText = fmt.Sprintf("Move #%s to another project", s.target.issueShortID)
		extra = []string{"Project: " + s.filter.View()}
		list = func(room int) []string { return renderMoveChoices(s, innerW, min(max(room, 1), 10)) }
	}
	var outcomeLines []string
	for _, line := range outcome {
		outcomeLines = append(outcomeLines, wrap(line)...)
	}

	// The richest title and outcome that leave room for what must stay. Under a
	// refusal even the picker's filter line gives way.
	titleLines, outLines := styled(wrap(titleText)), outcomeLines
	shortTitle := styled(oneLine(titleText))
	tries := []struct{ title, outcome, extra []string }{
		{titleLines, outcomeLines, extra},
		{titleLines, firstOrNil(outcome, oneLine), extra},
		{shortTitle, firstOrNil(outcome, oneLine), extra},
		{shortTitle, nil, extra},
	}
	if len(tail) > 1 {
		tries = append(tries, struct{ title, outcome, extra []string }{shortTitle, nil, nil})
	}
	for _, try := range tries {
		titleLines, outLines, extra = try.title, try.outcome, try.extra
		if len(titleLines)+len(outLines)+len(extra)+len(tail) <= budget {
			break
		}
	}
	// Still too much: the error text gives up its last lines, never the prompt.
	if over := len(titleLines) + len(outLines) + len(extra) + len(tail) - budget; over > 0 && s.err != "" {
		prompt := 0
		if !footerShown || s.sending {
			prompt = 1
		}
		errEnd := len(tail) - prompt
		cut := min(over, max(len(strings.Split(errorStyle.Width(max(innerW, 1)).Render(s.err), "\n"))-1, 0))
		tail = slices.Delete(tail, errEnd-cut, errEnd)
	}

	parts := append(append(append([]string{}, titleLines...), outLines...), extra...)
	room := budget - len(parts) - len(tail)
	if len(optional) > 0 && room >= len(optional)+2 {
		parts = append(parts, "")
		parts = append(parts, optional...)
		room -= len(optional) + 1
	}
	if list != nil && room > 0 {
		parts = append(parts, list(room)...)
	}
	parts = append(parts, tail...)
	body := strings.Join(parts, "\n")
	if !boxed {
		// With no border the lines would let the list show between them, so each
		// one clears its terminal row.
		return lipgloss.NewStyle().Width(max(width, 1)).Render(body)
	}
	return modalBoxStyle.Render(body)
}

// firstOrNil is the first of lines, cut to one row, or nothing.
func firstOrNil(lines []string, oneLine func(string) []string) []string {
	if len(lines) == 0 {
		return nil
	}
	return oneLine(lines[0])
}

func renderMoveChoices(s moveState, innerW, visible int) []string {
	if s.loading {
		return []string{subtleStyle.Render("loading projects…")}
	}
	choices := s.choices()
	if len(choices) == 0 {
		return []string{subtleStyle.Render("no project matches")}
	}
	cursor := min(s.cursor, len(choices)-1)
	start := max(cursor-visible+1, 0)
	var rows []string
	for i := start; i < min(start+visible, len(choices)); i++ {
		prefix := "  "
		if i == cursor {
			prefix = "▸ "
		}
		rows = append(rows, prefix+truncate(sanitizeForLine(choices[i].label), innerW-2))
	}
	return rows
}

package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	revision    int64
	uids        []string
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

func (m Model) routeMoveKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	if !m.keymap.MoveIssue.matches(msg) {
		return m, nil, false
	}
	// A pending undo or reopen owns the issue until it answers.
	if m.undoInFlight || m.reopenLookupPending {
		return m, nil, true
	}
	target, issue, ok := m.listIssueTarget()
	if !ok {
		return m, nil, true
	}
	mover, ok := m.api.(moveAPI)
	if !ok {
		next, cmd := m.undoNotice("this connection cannot move issues", toastError)
		return next, cmd, true
	}
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
			if area := jsonString(p.Metadata.Area); area != "" {
				areaCount[area]++
			}
		}
		for _, p := range group {
			if area := jsonString(p.Metadata.Area); area != "" && areaCount[area] == 1 {
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
	case "up", "ctrl+p":
		s.cursor = max(s.cursor-1, 0)
		m.move = s
	case "down", "ctrl+n":
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
		m.move = s.filterUpdated(msg)
	}
	return m, nil
}

// filterUpdated feeds a key or paste to the filter and returns to the first
// match when the text changed.
func (s moveState) filterUpdated(msg tea.Msg) moveState {
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
	m.move = m.move.filterUpdated(msg)
	return m
}

// dispatchMovePreview asks the daemon what moving to the chosen project would
// do. It sends no If-Match, so the preview reads the issue as it stands now.
func (m Model) dispatchMovePreview() (Model, tea.Cmd) {
	s := m.move
	s.seq++
	s.sending, s.err = true, ""
	m.move = s
	in := MoveInput{Actor: m.moveActor(), ToProjectUID: s.chosen.UID, DryRun: true}
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

func (m Model) moveActor() string {
	return m.list.actor
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
		s.err = "the daemon's preview did not report the issue's revision, so the move was not sent; press esc and choose the project again"
		m.move = s
		return m, nil
	}
	s.sending, s.err, s.note = true, "", ""
	m.move = s
	in := MoveInput{Actor: m.moveActor(), ToProjectUID: s.chosen.UID, IfMatchRevision: s.preview.revision}
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
		s.note = "The issue changed since the preview; nothing was moved. Showing the current preview."
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

// finishMove closes the form after a move. The moved issue and its subtree
// leave the list the way a deleted one does (a move into another project is
// listed again by the refetch when the scope shows it), the detail pane resets
// only when it shows one of them, and the notice names the new project#id.
// The daemon moved labels with the issues, so both projects' label counts are
// refreshed.
func (m Model) finishMove(mut mutationDoneMsg) (Model, tea.Cmd) {
	s := m.move
	uid := s.target.issueUID
	moved := append([]string{}, s.preview.uids...)
	newRef := s.chosen.Name
	if mut.resp != nil && mut.resp.NewShortID != "" {
		newRef += "#" + mut.resp.NewShortID
	}
	m = m.closeMoveForm()
	m.detail = m.detail.withoutIssues(append(moved, uid)...)
	m, follow := m.refreshListDay(func(lm listModel) listModel { return lm.withoutIssues(uid, moved...) })
	m, detailCmd := m.maybeBootstrapSplitDetail()
	if m.cache != nil {
		m.cache.markStale()
	}
	m, notice := m.undoNotice(sanitizeForLine(fmt.Sprintf("moved #%s to %s", s.target.issueShortID, newRef)), toastSuccess)
	cmds := []tea.Cmd{follow, detailCmd, notice, withConnGen(m.list.refetchCmd(m.api, m.scope), m.connGen)}
	for _, pid := range []int64{s.target.projectID, s.chosen.ID} {
		if m.projectLabels == nil {
			break
		}
		if _, cached := m.projectLabels.byProject[pid]; cached {
			var refresh tea.Cmd
			m, refresh = m.dispatchLabelFetch(pid)
			cmds = append(cmds, refresh)
		}
	}
	return m, tea.Batch(cmds...)
}

// renderMoveModal draws the project picker, or the daemon's preview once a
// project is chosen.
func renderMoveModal(s moveState, width, height int) string {
	innerW := max(min(width*7/10, 80), min(width-8, 30))
	wrap := func(text string) string { return lipgloss.NewStyle().Width(innerW).Render(text) }
	var parts []string
	if s.preview != nil || (s.sending && s.chosen.UID != "") {
		parts = append(parts, titleStyle.Render(sanitizeForLine(fmt.Sprintf("Move #%s to %s", s.target.issueShortID, s.chosen.Name))), "")
		if preview := s.preview; preview != nil {
			if len(preview.descendants) == 0 {
				parts = append(parts, wrap(fmt.Sprintf("#%s has no descendants, so it moves alone.", sanitizeForLine(s.target.issueShortID))))
			} else {
				noun := "descendants"
				if len(preview.descendants) == 1 {
					noun = "descendant"
				}
				parts = append(parts, wrap(fmt.Sprintf("Subtree move: #%s and its %d %s move together.",
					sanitizeForLine(s.target.issueShortID), len(preview.descendants), noun)))
				shown := min(len(preview.descendants), max(height-16, 3))
				for _, line := range preview.descendants[:shown] {
					parts = append(parts, "  "+truncate(line, innerW-2))
				}
				if more := len(preview.descendants) - shown; more > 0 {
					parts = append(parts, subtleStyle.Render(fmt.Sprintf("  … and %d more", more)))
				}
			}
			parts = append(parts, "", wrap("Nothing has moved yet. Each issue gets a new id in "+sanitizeForLine(s.chosen.Name)+"."))
		}
	} else {
		parts = append(parts, titleStyle.Render(sanitizeForLine(fmt.Sprintf("Move #%s to another project", s.target.issueShortID))))
		parts = append(parts, "Project: "+s.filter.View(), "")
		parts = append(parts, renderMoveChoices(s, innerW, max(min(height-12, 10), 3))...)
	}
	if s.note != "" {
		parts = append(parts, "", wrap(s.note))
	}
	if s.err != "" {
		parts = append(parts, "", errorStyle.Width(innerW).Render(s.err))
	}
	if s.sending {
		parts = append(parts, "", statusStyle.Render("working…"))
	}
	return modalBoxStyle.Render(strings.Join(parts, "\n"))
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

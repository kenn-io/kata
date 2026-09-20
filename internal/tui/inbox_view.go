package tui

import (
	"context"
	"encoding/json/v2"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kit/tui/splitlayout"
)

type inboxReturnState struct {
	scope    scope
	list     listModel
	detail   detailModel
	view     viewID
	prevView viewID
	focus    focusPane
}

type inboxProjectLoadedMsg struct {
	connGen uint64
	attempt uint64
	refresh bool
	project ProjectSummary
	found   bool
	err     error
}

func (m Model) toggleInbox() (Model, tea.Cmd) {
	if m.inboxReturn != nil {
		return m.leaveInbox()
	}
	if m.api == nil {
		return m, nil
	}
	return m.lookupInboxProject(false)
}

func (m Model) lookupInboxProject(refresh bool) (Model, tea.Cmd) {
	m.inboxAttempt++
	m.inboxPending = !refresh
	api, connGen, attempt := m.api, m.connGen, m.inboxAttempt
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		projects, err := api.ListProjects(ctx)
		project, found := designatedInboxProject(projects)
		return inboxProjectLoadedMsg{connGen: connGen, attempt: attempt, refresh: refresh, project: project, found: found, err: err}
	}
}

func designatedInboxProject(projects []ProjectSummary) (ProjectSummary, bool) {
	for _, project := range projects {
		var role string
		if project.ID > 0 && json.Unmarshal(project.Metadata.Role, &role) == nil && role == "inbox" {
			return project, true
		}
	}
	return ProjectSummary{}, false
}

// inboxReresolveNeeded reports whether an SSE event can move, clear,
// or rename the role=inbox designation and therefore warrants a fresh
// ListProjects lookup. That is so in two states: while the Inbox view
// is active, and while an I-key entry lookup is still pending.
//
// Pending entry: the lookup's ListProjects snapshot may predate the
// frame. Any designation-affecting event restarts the lookup, and the
// fresh reply's inboxAttempt fences the stale one. removed/renamed
// cannot be gated on m.scope.projectID here — the designated project
// is not known until a reply lands.
//
// Active Inbox: project.metadata_updated may grant, move, or clear the
// role on any project, so it is not gated on the event's project.
// project.removed (archive, including force) and project.renamed are
// attributed to their own project and only matter when that is the
// designated one. project.merged deletes the source project row but is
// attributed to the surviving target, so the frame alone cannot prove
// the Inbox project survived — the lookup runs for every merge.
// project.restored only clears deleted_at and keeps metadata, so an
// archived project holding role=inbox re-enters the designation scan
// when restored; its empty payload cannot prove whether the effective
// designation moved, so like merged the lookup runs for every restore.
func (m Model) inboxReresolveNeeded(msg eventReceivedMsg) bool {
	if m.inboxPending {
		switch msg.eventType {
		case "project.metadata_updated", "project.merged", "project.removed", "project.renamed", "project.restored":
			return true
		}
		return false
	}
	if !m.scope.inbox {
		return false
	}
	switch msg.eventType {
	case "project.metadata_updated", "project.merged", "project.restored":
		return true
	case "project.removed", "project.renamed":
		return msg.projectID == m.scope.projectID
	default:
		return false
	}
}

func (m Model) handleInboxProjectLoaded(msg inboxProjectLoadedMsg) (Model, tea.Cmd) {
	if m.staleConnMsg(msg.connGen) || msg.attempt != m.inboxAttempt {
		return m, nil
	}
	if msg.refresh && m.inboxReturn == nil || !msg.refresh && !m.inboxPending {
		return m, nil
	}
	if msg.refresh && !m.inboxScopeForeground() {
		// The lookup was in flight while the user opened a full-screen
		// view over the Inbox. Applying it now would leaveInbox or swap
		// scopes over a view the user is reading; defer the resolution
		// to the exit path that restores the Inbox list.
		m.inboxReresolvePending = true
		return m, nil
	}
	m.inboxPending = false
	if msg.err != nil {
		return m.inboxNotice("Could not load Inbox project: " + msg.err.Error())
	}
	if !msg.found {
		if msg.refresh {
			var fetch tea.Cmd
			m, fetch = m.leaveInbox()
			m, notice := m.inboxNotice("No Inbox project designated")
			return m, tea.Batch(fetch, notice)
		}
		return m.inboxNotice("No Inbox project designated")
	}
	if msg.refresh && m.scope.projectID == msg.project.ID {
		m.scope.projectName = msg.project.Name
		return m, nil
	}
	if !msg.refresh {
		m.inboxReturn = &inboxReturnState{scope: m.scope, list: m.list, detail: m.detail, view: m.view, prevView: m.prevView, focus: m.focus}
	}
	actor := m.list.actor
	m.inboxVisit++
	m.scope = scope{projectID: msg.project.ID, projectName: msg.project.Name, inbox: true,
		inboxVisit:    m.inboxVisit,
		homeProjectID: m.scope.homeProjectID, homeProjectName: m.scope.homeProjectName}
	m.list = newListModel()
	m.list.actor = actor
	m.list.inboxOnly = true
	m.list.viewMode = issueListViewFlat
	m.detail = newDetailModel()
	m.view = viewList
	m.focus = focusList
	m.cache.drop()
	m.pendingRefetch = false
	m.nextDetailFollowGen++
	return m, m.fetchInitial()
}

func (m Model) leaveInbox() (Model, tea.Cmd) {
	if m.inboxReturn == nil {
		return m, nil
	}
	previous := *m.inboxReturn
	m.inboxReturn = nil
	m.inboxReresolvePending = false
	// The restored scope must not share a cache key with fetches that were
	// in flight before this Inbox visit: the saved scope carries the
	// pre-entry inboxVisit, so a slow pre-Inbox reply would collide with
	// the restore fetch's key and overwrite the fresh rows. Stamp the next
	// nonce into the restored scope so every ordinary-scope era keys
	// distinctly. Wire filters are untouched — this only discriminates
	// client-side cache slots.
	m.inboxVisit++
	previous.scope.inboxVisit = m.inboxVisit
	m.scope, m.list, m.detail = previous.scope, previous.list, previous.detail
	m.view, m.prevView, m.focus = previous.view, previous.prevView, previous.focus
	m.cache.drop()
	m.pendingRefetch = false
	m.nextDetailFollowGen++
	var fetch tea.Cmd
	if m.view == viewList || m.view == viewDetail {
		fetch = m.fetchInitial()
	} else if m.scope.projectID != 0 || m.scope.allProjects {
		// The restored foreground (help, daemons, federation,
		// credentials, projects) hides the list, but the restored scope
		// still owns a queue that must converge with daemon state: fetch
		// it so issue changes made during the Inbox era are visible when
		// the user leaves the overlay, without another event. The
		// foreground view stays untouched.
		fetch = m.fetchInitial()
	}
	if refetch := m.restoredDetailRefetch(); refetch != nil {
		fetch = tea.Batch(fetch, refetch)
	}
	return m, fetch
}

// inboxScopeForeground reports whether the Inbox scope's list or detail
// is the active foreground. Full-screen views (help, projects, daemons,
// federation, credentials) hide the Inbox entirely; designation
// re-resolves must not disturb their foreground.
func (m Model) inboxScopeForeground() bool {
	return m.view == viewList || m.view == viewDetail
}

// resumeInboxReresolve re-runs the designation lookup deferred while a
// full-screen view covered the Inbox. Call it from a global-view exit
// path after the restored foreground is in place: it fires only when
// the Inbox scope's list or detail is the restored foreground and a
// designation event landed while it was covered.
func (m Model) resumeInboxReresolve() (Model, tea.Cmd) {
	if !m.scope.inbox || !m.inboxReresolvePending || !m.inboxScopeForeground() {
		return m, nil
	}
	m.inboxReresolvePending = false
	return m.lookupInboxProject(true)
}

// restoredDetailRefetch re-fetches the restored detail pane through the
// same dm.refetch path a mutation uses, so a detail saved before
// entering the Inbox converges with daemon state instead of showing
// whatever was current when the user left. The pane that matters is the
// one the restored foreground will reveal: viewDetail itself, any
// retained split-mode pane, or — when the restored foreground is a
// full-screen view such as Help or Daemons — the detail pane behind
// that overlay, revealed when it closes. Only a visible pane fetches:
// a stacked list restore keeps the detail pane hidden
// until Enter re-opens it with a fresh fetch anyway. The fetches land
// in the retained pane without touching the overlay foreground.
func (m Model) restoredDetailRefetch() tea.Cmd {
	if m.layout == splitlayout.Split {
		return m.detail.refetch(m.api)
	}
	foreground := m.view
	if !m.inboxScopeForeground() {
		foreground = m.prevView
	}
	if foreground != viewDetail {
		return nil
	}
	return m.detail.refetch(m.api)
}

func (m Model) inboxNotice(message string) (Model, tea.Cmd) {
	m.list.status = ""
	m.toast = &toast{text: message, level: toastInfo, expiresAt: m.toastNow().Add(toastNoBindingTTL)}
	return m, toastExpireCmd(toastNoBindingTTL)
}

func inboxOpenIssues(issues []Issue) []Issue {
	out := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Status == "open" {
			out = append(out, issue)
		}
	}
	return out
}

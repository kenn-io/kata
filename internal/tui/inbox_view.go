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
	return m.lookupInboxProject()
}

func (m Model) lookupInboxProject() (Model, tea.Cmd) {
	m.inboxAttempt++
	m.inboxPending = true
	api, connGen, attempt := m.api, m.connGen, m.inboxAttempt
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		projects, err := api.ListProjects(ctx)
		project, found := designatedInboxProject(projects)
		return inboxProjectLoadedMsg{connGen: connGen, attempt: attempt, project: project, found: found, err: err}
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

func (m Model) handleInboxProjectLoaded(msg inboxProjectLoadedMsg) (Model, tea.Cmd) {
	if m.staleConnMsg(msg.connGen) || msg.attempt != m.inboxAttempt {
		return m, nil
	}
	if !m.inboxPending {
		return m, nil
	}
	m.inboxPending = false
	if msg.err != nil {
		return m.inboxNotice("Could not load Inbox project: " + msg.err.Error())
	}
	if !msg.found {
		return m.inboxNotice("No Inbox project designated")
	}
	m.inboxReturn = &inboxReturnState{scope: m.scope, list: m.list, detail: m.detail, view: m.view, prevView: m.prevView, focus: m.focus}
	actor := m.list.actor
	m.scopeGen++
	m.scope = scope{projectID: msg.project.ID, projectName: msg.project.Name, inbox: true,
		scopeGen:      m.scopeGen,
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
	// The restored scope must not share a cache key with fetches that were
	// in flight before this Inbox visit: the saved scope carries the
	// pre-entry scopeGen, so a slow pre-Inbox reply would collide with
	// the restore fetch's key and overwrite the fresh rows. Stamp the next
	// nonce into the restored scope so every ordinary-scope era keys
	// distinctly. Wire filters are untouched — this only discriminates
	// client-side cache slots.
	m.scopeGen++
	previous.scope.scopeGen = m.scopeGen
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
	if m.view != viewList && m.view != viewDetail {
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

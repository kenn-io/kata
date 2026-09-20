package tui

import (
	"context"
	"encoding/json/jsontext"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/splitlayout"
)

type inboxTestAPI struct {
	KataAPI
	projects      []ProjectSummary
	issues        []Issue
	detail        *IssueDetail
	comments      []CommentEntry
	events        []EventLogEntry
	links         []LinkEntry
	listProjectID int64
	listStatus    string
	detailCalls   int
	commentCalls  int
	eventCalls    int
	linkCalls     int
}

func (a *inboxTestAPI) ListProjects(context.Context) ([]ProjectSummary, error) {
	return a.projects, nil
}

func (a *inboxTestAPI) ListIssues(_ context.Context, projectID int64, filter ListFilter) ([]Issue, error) {
	a.listProjectID = projectID
	a.listStatus = filter.Status
	return a.issues, nil
}

func (a *inboxTestAPI) ListAllIssues(_ context.Context, _ ListFilter) ([]Issue, error) {
	return a.issues, nil
}

func (a *inboxTestAPI) GetIssueDetail(_ context.Context, _ int64, _ string) (*IssueDetail, error) {
	a.detailCalls++
	return a.detail, nil
}

func (a *inboxTestAPI) ListComments(_ context.Context, _ int64, _ string) ([]CommentEntry, error) {
	a.commentCalls++
	return a.comments, nil
}

func (a *inboxTestAPI) ListEvents(_ context.Context, _ int64, _ string) ([]EventLogEntry, error) {
	a.eventCalls++
	return a.events, nil
}

func (a *inboxTestAPI) ListLinks(_ context.Context, _ int64, _ string) ([]LinkEntry, error) {
	a.linkCalls++
	return a.links, nil
}

// drainInboxCmds executes cmd and feeds every produced message back into
// m.Update, flattening nested tea.BatchMsg levels so a Batch(fetch,
// refetch) round-trip converges in one call.
func drainInboxCmds(m Model, cmd tea.Cmd) Model {
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, work := range batch {
			m = drainInboxCmds(m, work)
		}
		return m
	}
	m, _ = updateModel(m, msg)
	return m
}

func TestListProjectsRetainsInboxDesignation(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects", r.URL.Path)
		respondJSON(t, w, map[string]any{"projects": []map[string]any{
			{"id": 1, "name": "Inbox", "metadata": map[string]any{}},
			{"id": 2, "name": "example-project", "metadata": map[string]any{"role": "inbox"}},
			{"id": 3, "name": "spoke-project", "metadata": map[string]any{"role": 7}},
		}})
	})

	projects, err := client.ListProjects(context.Background())
	require.NoError(t, err)
	require.Len(t, projects, 3)
	selected, ok := designatedInboxProject(projects)
	require.True(t, ok)
	require.Equal(t, int64(2), selected.ID)
}

func TestInboxViewShowsDesignatedProjectOpenTasksAndRestoresList(t *testing.T) {
	useNoColor(t)
	actualInbox := ProjectSummary{ID: 2, Name: "example-project"}
	actualInbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "Inbox"}, actualInbox},
		issues: []Issue{
			{UID: "01TEST-open", ProjectID: 2, ShortID: "open", Title: "Capture task", Status: "open"},
			{UID: "01TEST-done", ProjectID: 2, ShortID: "done", Title: "Completed task", Status: "closed"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.list.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.list.filter.Search = "Prior"
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup, "I should resolve the designated Inbox project")
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch, "Inbox should load the selected project's tasks")
	m, _ = updateModel(m, fetch())
	require.Equal(t, int64(2), api.listProjectID)
	require.Equal(t, "open", api.listStatus)
	frame := stripANSI(m.viewContent())
	require.Contains(t, frame, "Inbox")
	require.Contains(t, frame, "Capture task")
	require.NotContains(t, frame, "Completed task")
	require.NotContains(t, frame, "Prior work")

	m, open := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, open)
	m, _ = updateModel(m, open())
	require.Equal(t, viewDetail, m.view)
	require.Equal(t, int64(2), m.detail.scopePID)
	m, _ = updateModel(m, popDetailMsg{})
	require.Contains(t, stripANSI(m.viewContent()), "Capture task")
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, "Prior", m.list.filter.Search)
	require.True(t, strings.Contains(stripANSI(m.viewContent()), "Prior work"))
}

func TestInboxViewRequiresRoleDesignation(t *testing.T) {
	api := &inboxTestAPI{projects: []ProjectSummary{{ID: 1, Name: "Inbox"}}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch, "the notice should expire")
	require.Zero(t, api.listProjectID, "a project named Inbox without the role must not load")
	require.Equal(t, int64(1), m.scope.projectID)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "Inbox project")
}

func TestInboxViewWorksWithoutBoundWorkspaceAndReturnsToProjects(t *testing.T) {
	project := ProjectSummary{ID: 2, Name: "example-project"}
	project.Metadata.Role = jsontext.Value(`"inbox"`)
	m := newTestModel()
	m.api = &inboxTestAPI{projects: []ProjectSummary{project}}
	m.scope = scope{empty: true}
	m.view = viewProjects
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	require.True(t, m.scope.inbox)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewProjects, m.view)
	require.True(t, m.scope.empty)
}

func TestInboxViewDropsPriorUnfilteredFetchForSameProject(t *testing.T) {
	m := newTestModel()
	m.scope = scope{projectID: 7, projectName: "example-project", inbox: true}
	m.list.inboxOnly = true
	m.list.issues = []Issue{{UID: "01TEST-new", ProjectID: 7, ShortID: "new", Title: "Inbox task", Status: "open"}}
	m, _ = updateModel(m, initialFetchMsg{
		dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit},
		issues:      []Issue{{UID: "01TEST-old", ProjectID: 7, ShortID: "old", Title: "Stale task", Status: "open"}},
	})
	require.Equal(t, "Inbox task", m.list.issues[0].Title)
}

func TestInboxViewDropsFetchFromEarlierVisit(t *testing.T) {
	project := ProjectSummary{ID: 7, Name: "example-project"}
	project.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{projects: []ProjectSummary{project}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m, firstLookup := updateModel(m, keyRune('I'))
	m, oldFetch := updateModel(m, firstLookup())
	api.issues = []Issue{{UID: "01TEST-old", ProjectID: 7, Title: "Old task", Status: "open"}}
	oldResult := oldFetch()
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	m, secondLookup := updateModel(m, keyRune('I'))
	m, newFetch := updateModel(m, secondLookup())
	api.issues = []Issue{{UID: "01TEST-new", ProjectID: 7, Title: "New task", Status: "open"}}
	m, _ = updateModel(m, newFetch())
	m, _ = updateModel(m, oldResult)
	require.Equal(t, "New task", m.list.issues[0].Title)
}

// TestInboxLeaveDropsPreInboxFetchArrivingAfterRestore pins the review
// finding: a project list fetch dispatched before entering the Inbox
// shares the restored scope's cache key (the saved scope keeps its
// pre-entry inboxVisit). When that reply lands after the fresh restore
// fetch it must be dropped, not overwrite the fresh rows and cache with
// the pre-Inbox snapshot.
func TestInboxLeaveDropsPreInboxFetchArrivingAfterRestore(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-pre", ProjectID: 1, ShortID: "pre", Title: "Pre-Inbox task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	// A project list fetch is in flight when the user enters the Inbox.
	oldFetch := m.fetchInitial()
	require.NotNil(t, oldFetch)

	api.issues = []Issue{
		{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
	}
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, inboxFetch := updateModel(m, lookup())
	require.NotNil(t, inboxFetch)
	m, _ = updateModel(m, inboxFetch())
	require.True(t, m.scope.inbox)

	// Daemon state moves while the Inbox is open.
	api.issues = []Issue{
		{UID: "01TEST-fresh", ProjectID: 1, ShortID: "fresh", Title: "Fresh task", Status: "open"},
	}
	m, back := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.False(t, m.scope.inbox)
	m = runInboxRestoreCmd(m, back)
	require.NotEmpty(t, m.list.issues)
	require.Equal(t, "Fresh task", m.list.issues[0].Title)

	// The pre-Inbox reply lands after the fresh restore result.
	api.issues = []Issue{
		{UID: "01TEST-pre", ProjectID: 1, ShortID: "pre", Title: "Pre-Inbox task", Status: "open"},
	}
	m = runInboxRestoreCmd(m, oldFetch)
	require.Equal(t, "Fresh task", m.list.issues[0].Title,
		"a pre-Inbox reply landing after the restore fetch must not overwrite the fresh list")
	require.NotEmpty(t, m.cache.data)
	require.Equal(t, "Fresh task", m.cache.data[0].Title,
		"a pre-Inbox reply landing after the restore fetch must not overwrite the cache")
}

// TestInboxEscCancelsPendingPriorityBeforeLeaving pins the review
// finding: after `!` arms the priority prompt (whose hint says "esc to
// cancel"), esc must cancel the prompt and keep the Inbox list active.
// Only a second esc — with the prompt disarmed — leaves the Inbox.
func TestInboxEscCancelsPendingPriorityBeforeLeaving(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-aaa1", ProjectID: 2, ShortID: "aaa1", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	// Enter the designated Inbox.
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, inboxFetch := updateModel(m, lookup())
	require.NotNil(t, inboxFetch)
	m, _ = updateModel(m, inboxFetch())
	require.True(t, m.scope.inbox)
	require.True(t, m.list.inboxOnly)

	// Arm the `!` priority prompt on the highlighted row.
	m, _ = updateModel(m, keyRune('!'))
	require.True(t, m.list.pendingPriority, "`!` must arm the pending priority prompt")
	require.NotEmpty(t, m.list.status)

	// First esc cancels the prompt and must not leave the Inbox.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.False(t, m.list.pendingPriority, "esc must cancel the pending prompt")
	require.Empty(t, m.list.status, "esc must clear the prompt hint")
	require.True(t, m.scope.inbox,
		"esc must not leave the Inbox while the priority prompt is pending")
	require.NotNil(t, m.inboxReturn, "the Inbox return state must survive the cancel")
	require.True(t, m.list.inboxOnly, "esc must not swap the Inbox list out")

	// A second esc, with the prompt disarmed, leaves the Inbox normally.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.False(t, m.scope.inbox)
	require.Nil(t, m.inboxReturn)
	require.Equal(t, int64(1), m.scope.projectID)
}

func TestInboxViewPreservesHelpReturnNavigation(t *testing.T) {
	project := ProjectSummary{ID: 7, Name: "example-project"}
	project.Metadata.Role = jsontext.Value(`"inbox"`)
	m := newTestModel()
	m.api = &inboxTestAPI{projects: []ProjectSummary{project}}
	m.view, m.prevView = viewHelp, viewDaemons
	m, lookup := updateModel(m, keyRune('I'))
	m, _ = updateModel(m, lookup())
	m, _ = updateModel(m, keyRune('?'))
	m, _ = updateModel(m, keyRune('?'))
	m, _ = updateModel(m, keyRune('I'))
	require.Equal(t, viewHelp, m.view)
	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewDaemons, m.view)
}

func TestInboxNoticeVisibleOverOldStatus(t *testing.T) {
	useNoColor(t)
	m := newTestModel()
	m.api = &inboxTestAPI{projects: []ProjectSummary{{ID: 1, Name: "Inbox"}}}
	m.list.status = "old status"
	m, lookup := updateModel(m, keyRune('I'))
	m, _ = updateModel(m, lookup())
	line := stripANSI(renderListInfoLine(100, m.chrome(), m.list, 10))
	require.Contains(t, line, "No Inbox project designated")
}

func TestInboxViewFollowsLiveDesignationChange(t *testing.T) {
	first := ProjectSummary{ID: 7, Name: "example-project"}
	first.Metadata.Role = jsontext.Value(`"inbox"`)
	second := ProjectSummary{ID: 8, Name: "spoke-project"}
	second.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{projects: []ProjectSummary{first}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "hub-project")
	m, lookup := updateModel(m, keyRune('I'))
	m, _ = updateModel(m, lookup())
	require.Equal(t, int64(7), m.scope.projectID)
	api.projects = []ProjectSummary{second}
	m.sseCh = nil
	out, cmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 8})
	m = out.(Model)
	require.NotNil(t, cmd)
	result := cmd()
	if batch, ok := result.(tea.BatchMsg); ok {
		for _, work := range batch {
			m, cmd = updateModel(m, work())
		}
	} else {
		m, cmd = updateModel(m, result)
	}
	require.Equal(t, int64(8), m.scope.projectID)
	require.True(t, m.scope.inbox)
	require.NotNil(t, cmd)
}

// TestInboxLeaveRefetchesRestoredDetail pins the review finding: leaving
// the Inbox restores the saved detail pane, and that pane must converge
// with daemon state through the same detail.refetch path a mutation uses
// — issue, comments, events, and links.
func TestInboxLeaveRefetchesRestoredDetail(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	stale := testIssue("aaa1", withTitle("Stale title"))
	api := &inboxTestAPI{projects: []ProjectSummary{inbox}, issues: []Issue{stale}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	m.view = viewDetail
	m.detail = detailModel{issue: &stale, scopePID: 1}
	m.detail.comments = []CommentEntry{{ID: 1, Author: "avery", Body: "old comment"}}

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)

	// Daemon state moves while the Inbox is open.
	fresh := testIssue("aaa1", withTitle("Fresh title"))
	api.detail = &IssueDetail{Issue: &fresh}
	api.comments = []CommentEntry{
		{ID: 1, Author: "avery", Body: "old comment"},
		{ID: 2, Author: "kai", Body: "new comment"},
	}
	api.events = []EventLogEntry{{ID: 9, Type: "issue.updated"}}
	api.links = []LinkEntry{{ID: 4, Type: "related"}}

	m, back := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewDetail, m.view)
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, "Stale title", m.detail.issue.Title, "the saved pane is restored as-is until the refetch lands")

	m = drainInboxCmds(m, back)
	require.Positive(t, api.detailCalls, "leaving Inbox must refetch the restored issue")
	require.Positive(t, api.commentCalls, "leaving Inbox must refetch the restored comments")
	require.Positive(t, api.eventCalls, "leaving Inbox must refetch the restored events")
	require.Positive(t, api.linkCalls, "leaving Inbox must refetch the restored links")
	require.Equal(t, "Fresh title", m.detail.issue.Title)
	require.Len(t, m.detail.comments, 2)
	require.Len(t, m.detail.events, 1)
	require.Len(t, m.detail.links, 1)
}

// TestInboxLeaveRefetchesRestoredSplitDetail covers the split-layout
// return path: the restored view is the list, but the detail pane stays
// visible next to it and must refetch the same way.
func TestInboxLeaveRefetchesRestoredSplitDetail(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	stale := testIssue("aaa1", withTitle("Stale title"))
	api := &inboxTestAPI{projects: []ProjectSummary{inbox}, issues: []Issue{stale}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	m.layout = splitlayout.Split
	m.view = viewList
	m.focus = focusList
	m.detail = detailModel{issue: &stale, scopePID: 1}

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)

	fresh := testIssue("aaa1", withTitle("Fresh title"))
	api.detail = &IssueDetail{Issue: &fresh}

	m, back := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewList, m.view)
	require.Equal(t, splitlayout.Split, m.layout)
	m = drainInboxCmds(m, back)
	require.Positive(t, api.detailCalls, "the split layout keeps the restored detail pane visible; it must refetch")
	require.Equal(t, "Fresh title", m.detail.issue.Title)
}

// deliverInboxLifecycleEvent runs the command returned by
// handleEventReceived for a project lifecycle frame (a designation
// metadata update, archive/removal, merge, or rename), feeding every
// produced message back into the model and returning the command the
// inboxProjectLoadedMsg handler produced (the leave/notice batch). The
// debounced-refetch tick in the event batch runs for real (150ms), which
// lands after the restore and is a no-op against the freshly dropped
// cache.
func deliverInboxLifecycleEvent(m Model, eventCmd tea.Cmd) (Model, tea.Cmd) {
	if eventCmd == nil {
		return m, nil
	}
	var msgs []tea.Msg
	if result := eventCmd(); result != nil {
		if batch, ok := result.(tea.BatchMsg); ok {
			for _, work := range batch {
				if msg := work(); msg != nil {
					msgs = append(msgs, msg)
				}
			}
		} else {
			msgs = append(msgs, result)
		}
	}
	var loadedCmd tea.Cmd
	for _, msg := range msgs {
		var cmd tea.Cmd
		m, cmd = updateModel(m, msg)
		if _, ok := msg.(inboxProjectLoadedMsg); ok {
			loadedCmd = cmd
		}
	}
	return m, loadedCmd
}

// runInboxRestoreCmd executes a command chain from the designation-
// removed branch, feeding produced messages back into the model. The
// notice's toast-expiry command is a 3s tea.Tick timer, so commands run
// with a timeout and timer-backed ones are skipped instead of blocking
// the test for their full TTL.
func runInboxRestoreCmd(m Model, cmd tea.Cmd) Model {
	if cmd == nil {
		return m
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, work := range batch {
				m = runInboxRestoreCmd(m, work)
			}
			return m
		}
		next, nextCmd := updateModel(m, msg)
		return runInboxRestoreCmd(next, nextCmd)
	case <-time.After(time.Second):
		return m
	}
}

// TestInboxViewLeavesInboxWhenDesignationRemoved pins the review
// finding: a project.metadata_updated that clears the role=inbox
// designation while the Inbox is open must leave the Inbox, restore the
// exact prior scope and list, surface the "No Inbox project designated"
// notice, and still fire the restore fetch for the pre-Inbox list.
func TestInboxViewLeavesInboxWhenDesignationRemoved(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.list.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.list.filter.Search = "Prior"
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)
	require.Equal(t, int64(2), m.scope.projectID)
	require.Contains(t, stripANSI(m.viewContent()), "Capture task")

	// The daemon clears the designation while the Inbox is open and the
	// SSE stream delivers the metadata update for the de-designated
	// project.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	api.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd)
	m, leaveCmd := deliverInboxLifecycleEvent(m, eventCmd)

	require.Nil(t, m.inboxReturn, "a removed designation must clear the saved return state")
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, "spoke-project", m.scope.projectName)
	require.Equal(t, viewList, m.view)
	require.Equal(t, "Prior", m.list.filter.Search, "the saved list filter is restored")
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")
	require.Contains(t, stripANSI(m.viewContent()), "No Inbox project designated",
		"the notice must be visible right after the removal lands")

	m = runInboxRestoreCmd(m, leaveCmd)
	require.Equal(t, int64(1), api.listProjectID, "leaving must refetch the restored project's list")
	require.Equal(t, "", api.listStatus,
		"the restore fetch uses the restored filter, not the inbox-pinned open status")
	require.NotEmpty(t, m.list.issues)
	require.Equal(t, "Prior work", m.list.issues[0].Title)
}

// TestInboxViewDesignationRemovalRestoresNonListViewWithRefresh covers
// the removal branch when the saved view is not a list or detail (help
// here): the leave must keep the Help foreground, surface the notice,
// and still refresh the hidden restored scope so issue changes made
// during the Inbox era are visible when the user exits Help.
func TestInboxViewDesignationRemovalRestoresNonListViewWithRefresh(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.view, m.prevView = viewHelp, viewDaemons
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)

	// The daemon clears the designation while Help hides the list; the
	// restored scope's rows move in the same stroke.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	api.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd)
	m, leaveCmd := deliverInboxLifecycleEvent(m, eventCmd)

	require.NotNil(t, leaveCmd, "the notice expiry must still be scheduled")
	require.Nil(t, m.inboxReturn)
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, viewHelp, m.view)
	require.Equal(t, viewDaemons, m.prevView)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")

	m = runInboxRestoreCmd(m, leaveCmd)
	require.Equal(t, viewHelp, m.view,
		"the refresh fetch must not close the restored foreground view")
	require.Equal(t, int64(1), api.listProjectID,
		"the hidden restored scope's list must be fetched")
	require.Equal(t, "Prior work", m.list.issues[0].Title)
}

// TestInboxViewLeavesInboxWhenProjectForceArchived pins the review
// finding: force-archiving the active Inbox project emits
// project.removed, and a designation lookup that ran only for
// project.metadata_updated never fired — the TUI stayed on the archived
// project where every issue refetch fails. The Inbox must re-resolve,
// return to the prior view, and surface the "No Inbox project
// designated" notice.
func TestInboxViewLeavesInboxWhenProjectForceArchived(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.list.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.list.filter.Search = "Prior"
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)
	require.Equal(t, int64(2), m.scope.projectID)

	// The daemon archives the project — ListProjects excludes archived
	// rows — and delivers project.removed attributed to it.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	api.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.removed", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd, "archiving the active Inbox project must trigger re-resolution")
	m, leaveCmd := deliverInboxLifecycleEvent(m, eventCmd)

	require.Nil(t, m.inboxReturn, "archiving the active project must clear the saved return state")
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, "spoke-project", m.scope.projectName)
	require.Equal(t, viewList, m.view)
	require.Equal(t, "Prior", m.list.filter.Search, "the saved list filter is restored")
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")
	require.Contains(t, stripANSI(m.viewContent()), "No Inbox project designated",
		"the notice must be visible right after the archive lands")

	m = runInboxRestoreCmd(m, leaveCmd)
	require.Equal(t, int64(1), api.listProjectID, "leaving must refetch the restored project's list")
	require.Equal(t, "", api.listStatus,
		"the restore fetch uses the restored filter, not the inbox-pinned open status")
	require.NotEmpty(t, m.list.issues)
	require.Equal(t, "Prior work", m.list.issues[0].Title)
}

// TestInboxViewLeavesInboxWhenInboxProjectMergedAway covers the merge
// edge: merging the Inbox project into another deletes the source row,
// and the daemon attributes project.merged to the surviving target —
// the frame cannot be scoped to the removed project, so the lookup
// must run for every merge while the Inbox is open.
func TestInboxViewLeavesInboxWhenInboxProjectMergedAway(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.list.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)

	// project.merged is attributed to the surviving target (9); the
	// source row — the active Inbox project — is gone from ListProjects.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}, {ID: 9, Name: "merged-target"}}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.merged", projectID: 9})
	m = out.(Model)
	require.NotNil(t, eventCmd, "a merge away from the active Inbox project must trigger re-resolution")
	m, leaveCmd := deliverInboxLifecycleEvent(m, eventCmd)

	require.Nil(t, m.inboxReturn)
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, viewList, m.view)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")

	m = runInboxRestoreCmd(m, leaveCmd)
	require.Equal(t, int64(1), api.listProjectID)
}

// TestInboxRefreshUpdatesVisibleNameOnRename pins the rename edge: a
// project.renamed frame for the designated project keeps the
// designation, so the refresh lookup takes the same-project early
// return — which must still refresh scope.projectName so the visible
// header tracks the daemon name instead of going stale.
func TestInboxRefreshUpdatesVisibleNameOnRename(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)
	require.Equal(t, "example-project", m.scope.projectName)

	renamed := ProjectSummary{ID: 2, Name: "renamed-project"}
	renamed.Metadata.Role = jsontext.Value(`"inbox"`)
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}, renamed}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.renamed", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd, "a rename of the active Inbox project must trigger re-resolution")
	m, _ = deliverInboxLifecycleEvent(m, eventCmd)

	require.NotNil(t, m.inboxReturn, "a rename keeps the designation; the Inbox stays open")
	require.True(t, m.scope.inbox)
	require.Equal(t, int64(2), m.scope.projectID)
	require.Equal(t, "renamed-project", m.scope.projectName,
		"the same-project refresh must update the visible project name")
	require.Contains(t, stripANSI(m.viewContent()), "renamed-project",
		"the visible header must show the renamed project")
}

// TestInboxViewStaysWhenUnrelatedProjectRemoved pins the scope gate on
// the archive re-resolution: only the designated project's own removal
// can clear the designation, so a project.removed frame for another
// project must not leave the Inbox or surface the notice.
func TestInboxViewStaysWhenUnrelatedProjectRemoved(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)

	m.sseCh = nil
	out, _ := m.handleEventReceived(eventReceivedMsg{eventType: "project.removed", projectID: 9})
	m = out.(Model)

	require.NotNil(t, m.inboxReturn, "an unrelated removal must not clear the saved return state")
	require.True(t, m.scope.inbox)
	require.Equal(t, int64(2), m.scope.projectID)
	require.Nil(t, m.toast)
}

// TestInboxStatusCycleUnavailable pins the review finding: the Inbox
// fetches open issues only, so `s` must not cycle the status axis
// toward closed/all — it stays a no-op with a hint instead.
func TestInboxStatusCycleUnavailable(t *testing.T) {
	api, km, sc := newListEnv()
	sc.inbox = true
	lm := listModel{inboxOnly: true, issues: []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Status: "open"},
		{UID: "01TEST-bbb2", ShortID: "bbb2", Status: "closed"},
	}}
	for i := range 2 {
		var cmd tea.Cmd
		lm, cmd = lm.Update(runeKey('s'), km, api, sc)
		require.Nil(t, cmd)
		require.Equal(t, "", lm.filter.Status,
			"step %d: Inbox has no status axis; s must not cycle it", i)
		require.Equal(t, "Inbox shows open issues only", lm.status)
	}
}

// TestInboxFilterFormOmitsStatusAxis pins the review finding: the Inbox
// filter form must drop the Status radio entirely (no closed/all
// choice) while keeping the Owner, Search, and Labels axes intact, and
// committing it must not introduce a status filter.
func TestInboxFilterFormOmitsStatusAxis(t *testing.T) {
	useNoColor(t)
	m := filterFormFixture()
	m.scope = scope{projectID: 7, projectName: "kata", inbox: true}
	m.list.inboxOnly = true
	m.list.filter = ListFilter{Owner: "avery", Search: "login", Labels: []string{"bug"}}
	m = openFilterForm(t, m)

	require.Nil(t, m.input.field(fieldStatus),
		"Inbox fetches open issues only; the filter form must not offer the status axis")
	for _, id := range []fieldID{fieldOwner, fieldSearch, fieldLabels} {
		require.NotNil(t, m.input.field(id), "Inbox filter form must preserve the %v axis", id)
	}
	frame := stripANSI(renderCenteredForm(m.input, 120, 30))
	assertNotContains(t, frame, "closed", "Inbox filter form must not offer closed")
	assertContains(t, frame, "Owner", "Inbox filter form must keep the owner axis")
	assertContains(t, frame, "Search", "Inbox filter form must keep the search axis")
	assertContains(t, frame, "Labels", "Inbox filter form must keep the labels axis")

	out, _ := m.commitFilterForm(m.input)
	require.True(t, listFilterEqual(out.list.filter,
		ListFilter{Owner: "avery", Search: "login", Labels: []string{"bug"}}),
		"committing the Inbox filter form must preserve owner/search/labels and set no status")
}

// TestInboxFooterHidesStatusKey keeps the footer honest about the
// unavailable axis: the Inbox footer must not advertise `s status`.
func TestInboxFooterHidesStatusKey(t *testing.T) {
	inbox := Model{list: listModel{inboxOnly: true}}
	assertHelpItemAbsent(t, flattenHelpRows(inbox.queueHelpRows()),
		helplayout.HelpItem{Key: "s", Description: "status"})

	other := Model{}
	assertHelpItemPresent(t, flattenHelpRows(other.queueHelpRows()),
		helplayout.HelpItem{Key: "s", Description: "status"})
}

// enterInboxForTest enters the Inbox scope through the real I-key path
// and asserts the scope landed on the designated project.
func enterInboxForTest(t *testing.T, m Model) Model {
	t.Helper()
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, fetch := updateModel(m, lookup())
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.True(t, m.scope.inbox)
	return m
}

// inFlightInboxLookup extracts the designation-lookup reply from an
// event batch without delivering it, modeling a lookup whose reply is
// still in flight when the user navigates elsewhere.
func inFlightInboxLookup(t *testing.T, eventCmd tea.Cmd) inboxProjectLoadedMsg {
	t.Helper()
	result := eventCmd()
	if batch, ok := result.(tea.BatchMsg); ok {
		for _, work := range batch {
			if msg, ok := work().(inboxProjectLoadedMsg); ok {
				return msg
			}
		}
		t.Fatal("no inboxProjectLoadedMsg in the event batch")
		return inboxProjectLoadedMsg{}
	}
	msg, ok := result.(inboxProjectLoadedMsg)
	require.True(t, ok, "expected the designation lookup reply, got %T", result)
	return msg
}

// TestInboxDesignationEventDuringProjectsKeepsForegroundUntilExit pins
// the review finding: a designation-clearing event landing while the
// user browses Projects on top of the Inbox scope must not resolve
// then — leaveInbox would close the foreground Projects view. The
// re-resolve waits until a global-view exit path restores the Inbox
// list, and only then leaves the Inbox with the usual notice and
// restore fetch.
func TestInboxDesignationEventDuringProjectsKeepsForegroundUntilExit(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.list.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.width, m.height = 120, 30

	m = enterInboxForTest(t, m)
	m, _ = updateModel(m, keyRune('P'))
	require.Equal(t, viewProjects, m.view)
	returnState := m.inboxReturn
	require.NotNil(t, returnState)

	// The daemon clears the designation while Projects is foreground.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	api.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd)
	m, loadedCmd := deliverInboxLifecycleEvent(m, eventCmd)

	require.Nil(t, loadedCmd, "no designation lookup may resolve while a global view is foreground")
	require.Equal(t, viewProjects, m.view, "a designation event must not close the foreground Projects view")
	require.True(t, m.scope.inbox, "the Inbox scope stays active under the foreground view")
	require.Same(t, returnState, m.inboxReturn, "the saved return state must be preserved")
	require.Nil(t, m.toast, "the leave notice waits until the user returns to the Inbox")
	require.Equal(t, "Capture task", m.list.issues[0].Title, "the covered Inbox list keeps its rows")

	// Exiting Projects returns to the Inbox list and re-resolves.
	m, resume := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewList, m.view)
	require.NotNil(t, resume, "returning to the Inbox must re-resolve the designation")
	m, leaveCmd := deliverInboxLifecycleEvent(m, resume)
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, viewList, m.view)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")

	m = runInboxRestoreCmd(m, leaveCmd)
	require.Equal(t, int64(1), api.listProjectID, "the restored project's list is fetched")
	require.Equal(t, "Prior work", m.list.issues[0].Title)
}

// TestInboxInFlightReresolveAfterNavigationKeepsForeground pins the
// review finding: a designation lookup begun while the Inbox list is
// foreground can land after the user opened Projects. The late reply
// must defer to the foreground view, and the exit-path re-resolve must
// follow the moved designation while preserving the original return
// state.
func TestInboxInFlightReresolveAfterNavigationKeepsForeground(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	next := ProjectSummary{ID: 8, Name: "spoke-project"}
	next.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "hub-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "hub-project")
	m.width, m.height = 120, 30

	m = enterInboxForTest(t, m)

	// The designation moves while the Inbox list is foreground; the
	// re-resolve lookup is dispatched but its reply is still in flight
	// when the user opens Projects.
	api.projects = []ProjectSummary{{ID: 1, Name: "hub-project"}, next}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 8})
	m = out.(Model)
	require.NotNil(t, eventCmd)
	loaded := inFlightInboxLookup(t, eventCmd)
	returnState := m.inboxReturn

	m, _ = updateModel(m, keyRune('P'))
	require.Equal(t, viewProjects, m.view)

	// The in-flight reply lands after the navigation.
	m, _ = updateModel(m, loaded)
	require.Equal(t, viewProjects, m.view, "an in-flight lookup must not close the foreground view")
	require.True(t, m.scope.inbox)
	require.Equal(t, int64(2), m.scope.projectID)
	require.Same(t, returnState, m.inboxReturn)

	// Exiting Projects re-resolves and follows the moved designation.
	m, resume := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewList, m.view)
	require.NotNil(t, resume)
	m, _ = deliverInboxLifecycleEvent(m, resume)
	require.True(t, m.scope.inbox, "the moved designation keeps the Inbox open")
	require.Equal(t, int64(8), m.scope.projectID)
	require.Same(t, returnState, m.inboxReturn, "the original return state survives the re-entry")
	require.Equal(t, viewList, m.view)
}

// TestInboxRenameDuringProjectsDefersNameRefresh covers the rename
// event under the foreground rule: a rename of the designated project
// keeps the designation, but the same-project name refresh must wait
// for the exit-path re-resolve instead of mutating the covered scope
// while Projects is foreground.
func TestInboxRenameDuringProjectsDefersNameRefresh(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	m = enterInboxForTest(t, m)
	m, _ = updateModel(m, keyRune('P'))
	require.Equal(t, viewProjects, m.view)

	renamed := ProjectSummary{ID: 2, Name: "renamed-project"}
	renamed.Metadata.Role = jsontext.Value(`"inbox"`)
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}, renamed}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.renamed", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd)
	m, _ = deliverInboxLifecycleEvent(m, eventCmd)

	require.Equal(t, viewProjects, m.view, "a rename must not close the foreground Projects view")
	require.True(t, m.scope.inbox)
	require.Equal(t, "example-project", m.scope.projectName,
		"the name refresh waits for the exit-path re-resolve")

	m, resume := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewList, m.view)
	require.NotNil(t, resume)
	m, _ = deliverInboxLifecycleEvent(m, resume)
	require.True(t, m.scope.inbox, "a rename keeps the designation; the Inbox stays open")
	require.Equal(t, int64(2), m.scope.projectID)
	require.Equal(t, "renamed-project", m.scope.projectName,
		"the exit-path re-resolve must refresh the visible project name")
}

// TestInboxLeaveRefreshesRestoredScopeUnderHelp pins the restore-
// convergence finding: when the Inbox was entered from Help, leaving
// restores the Help screen and must also fetch the restored scope's
// list so issue changes made during the Inbox era are visible when the
// user exits Help — without another event and without closing the
// foreground view.
func TestInboxLeaveRefreshesRestoredScopeUnderHelp(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.list.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	// Help entered from the list: exiting Help (? from viewHelp) returns
	// to the restored list view.
	m.view, m.prevView = viewHelp, viewList
	m.width, m.height = 120, 30

	m = enterInboxForTest(t, m)

	// Daemon state for the restored scope moves during the Inbox era.
	api.issues = []Issue{{UID: "01TEST-fresh", ProjectID: 1, ShortID: "fresh", Title: "Fresh task", Status: "open"}}

	m, back := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewHelp, m.view, "leaving Inbox must restore, not close, the foreground Help view")
	require.Equal(t, int64(1), m.scope.projectID)
	require.NotNil(t, back, "the hidden restored scope must be refreshed on leave")

	m = runInboxRestoreCmd(m, back)
	require.Equal(t, viewHelp, m.view, "the fetch must not close the foreground view")
	require.Equal(t, "Fresh task", m.list.issues[0].Title,
		"the restored scope converges while Help stays foreground")

	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewList, m.view)
	require.Contains(t, stripANSI(m.viewContent()), "Fresh task",
		"exiting Help must show the fresh rows without waiting for an event")
}

// TestInboxLeaveRefreshesHiddenScopeFromEachOverlayEntry covers the
// same restore-convergence contract for every overlay screen the Inbox
// can be entered from with a non-list saved view: Help, Daemons, and
// Federation. Credentials is absent by construction — the I-key path
// runs prepareCredentialsGlobalExit first, so entering the Inbox from
// the credentials screen records the view underneath it and restores
// through the ordinary list path. Leaving must keep the restored
// foreground and fetch the hidden scope's list.
func TestInboxLeaveRefreshesHiddenScopeFromEachOverlayEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		view viewID
		prev viewID
	}{
		{"help", viewHelp, viewDaemons},
		{"daemons", viewDaemons, viewList},
		{"federation", viewFederation, viewList},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := ProjectSummary{ID: 2, Name: "example-project"}
			inbox.Metadata.Role = jsontext.Value(`"inbox"`)
			api := &inboxTestAPI{
				projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
				issues: []Issue{
					{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
				},
			}
			m := newTestModel()
			m.api = api
			m.scope = homedScope(1, "spoke-project")
			m.view, m.prevView = tc.view, tc.prev
			m.width, m.height = 120, 30

			m = enterInboxForTest(t, m)
			api.issues = []Issue{{UID: "01TEST-fresh", ProjectID: 1, ShortID: "fresh", Title: "Fresh task", Status: "open"}}

			m, back := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
			require.Equal(t, tc.view, m.view, "leaving must restore, not close, the overlay foreground")
			require.Equal(t, tc.prev, m.prevView)
			require.NotNil(t, back, "the hidden restored scope must be refreshed on leave")

			m = runInboxRestoreCmd(m, back)
			require.Equal(t, tc.view, m.view)
			require.Equal(t, int64(1), api.listProjectID,
				"the hidden restored scope's list must be fetched")
			require.Equal(t, "Fresh task", m.list.issues[0].Title)
		})
	}
}

// TestInboxLifecycleEventsDuringProjectsDeferUntilExit pins the
// foreground rule for the remaining designation-affecting event types:
// archive (project.removed) and merge (project.merged, attributed to
// the surviving target) must defer like metadata updates while a
// global view covers the Inbox, and resolve on the exit path.
func TestInboxLifecycleEventsDuringProjectsDeferUntilExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		event    eventReceivedMsg
		projects []ProjectSummary
	}{
		{
			name:     "archive",
			event:    eventReceivedMsg{eventType: "project.removed", projectID: 2},
			projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}},
		},
		{
			name:     "merge",
			event:    eventReceivedMsg{eventType: "project.merged", projectID: 9},
			projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, {ID: 9, Name: "merged-target"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useNoColor(t)
			inbox := ProjectSummary{ID: 2, Name: "example-project"}
			inbox.Metadata.Role = jsontext.Value(`"inbox"`)
			api := &inboxTestAPI{
				projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
				issues: []Issue{
					{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
				},
			}
			m := newTestModel()
			m.api = api
			m.scope = homedScope(1, "spoke-project")
			m.width, m.height = 120, 30

			m = enterInboxForTest(t, m)
			m, _ = updateModel(m, keyRune('P'))
			require.Equal(t, viewProjects, m.view)
			returnState := m.inboxReturn
			require.NotNil(t, returnState)

			api.projects = tc.projects
			m.sseCh = nil
			out, eventCmd := m.handleEventReceived(tc.event)
			m = out.(Model)
			require.NotNil(t, eventCmd)
			m, loadedCmd := deliverInboxLifecycleEvent(m, eventCmd)

			require.Nil(t, loadedCmd, "a lifecycle event must not resolve while Projects is foreground")
			require.Equal(t, viewProjects, m.view)
			require.True(t, m.scope.inbox)
			require.Same(t, returnState, m.inboxReturn)
			require.Nil(t, m.toast)

			m, resume := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
			require.Equal(t, viewList, m.view)
			require.NotNil(t, resume)
			m, leaveCmd := deliverInboxLifecycleEvent(m, resume)
			require.False(t, m.scope.inbox)
			require.Equal(t, int64(1), m.scope.projectID)
			require.NotNil(t, m.toast)
			require.Contains(t, m.toast.text, "No Inbox project designated")

			m = runInboxRestoreCmd(m, leaveCmd)
			require.Equal(t, int64(1), api.listProjectID)
		})
	}
}

// TestInboxDeferredReresolveFiresOnEachGlobalViewExit covers every
// global-view exit path: with a designation event deferred while the
// overlay was up, the exit must restore the Inbox list as foreground
// and re-resolve the designation through a fresh lookup.
func TestInboxDeferredReresolveFiresOnEachGlobalViewExit(t *testing.T) {
	escKey := tea.KeyPressMsg{Code: tea.KeyEsc}
	for _, tc := range []struct {
		name string
		view viewID
		prev viewID
		key  tea.KeyPressMsg
	}{
		{"projects esc", viewProjects, viewList, escKey},
		{"daemons esc", viewDaemons, viewList, escKey},
		{"federation esc", viewFederation, viewList, escKey},
		{"credentials esc", viewCredentials, viewList, escKey},
		{"help toggle", viewHelp, viewList, keyRune('?')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := ProjectSummary{ID: 2, Name: "example-project"}
			inbox.Metadata.Role = jsontext.Value(`"inbox"`)
			api := &inboxTestAPI{
				projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
				issues: []Issue{
					{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
				},
			}
			m := newTestModel()
			m.api = api
			m.scope = homedScope(1, "spoke-project")
			m.width, m.height = 120, 30

			m = enterInboxForTest(t, m)
			m.view, m.prevView = tc.view, tc.prev

			// The daemon clears the designation while the overlay is up.
			api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
			m.sseCh = nil
			out, _ := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 2})
			m = out.(Model)

			m, resume := updateModel(m, tc.key)
			require.Equal(t, viewList, m.view, "the exit path must restore the Inbox list")
			require.NotNil(t, resume, "the exit path must re-resolve the deferred designation")

			m, leaveCmd := deliverInboxLifecycleEvent(m, resume)
			require.False(t, m.scope.inbox)
			require.Equal(t, int64(1), m.scope.projectID)
			require.NotNil(t, m.toast)
			require.Contains(t, m.toast.text, "No Inbox project designated")

			m = runInboxRestoreCmd(m, leaveCmd)
			require.Equal(t, int64(1), api.listProjectID,
				"the restored project's list is fetched after the deferred leave")
		})
	}
}

// inboxCmdMsgs flattens cmd into the messages its leaves produce so a
// test controls the order they land. Leaves that do not answer within
// the runInboxRestoreCmd timeout (timer-backed toast ticks) are skipped
// instead of blocking the test for their full TTL.
func inboxCmdMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, work := range batch {
				out = append(out, inboxCmdMsgs(work)...)
			}
			return out
		}
		return []tea.Msg{msg}
	case <-time.After(time.Second):
		return nil
	}
}

// TestInboxLeaveRefetchesRetainedDetailBehindHelp pins the review
// finding: leaving the Inbox restores a saved full-screen view (Help)
// that keeps the detail pane retained behind it. The restored-detail
// refetch used to fire only when the restored view was list/detail, so
// after the user closed Help the retained issue, comments, events, and
// links could still show their pre-Inbox state. The refetch must run at
// leave time — landing in the retained pane without closing Help — so
// exiting Help reveals converged data.
func TestInboxLeaveRefetchesRetainedDetailBehindHelp(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	stale := testIssue("aaa1", withTitle("Stale title"))
	api := &inboxTestAPI{projects: []ProjectSummary{inbox}, issues: []Issue{stale}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	m.view = viewDetail
	m.detail = detailModel{issue: &stale, scopePID: 1}
	m.detail.comments = []CommentEntry{{ID: 1, Author: "avery", Body: "old comment"}}

	// Help covers the detail pane, and the Inbox opens on top of Help.
	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewHelp, m.view)
	require.Equal(t, viewDetail, m.prevView)
	m = enterInboxForTest(t, m)

	// Daemon state moves while the Inbox covers everything.
	fresh := testIssue("aaa1", withTitle("Fresh title"))
	api.detail = &IssueDetail{Issue: &fresh}
	api.comments = []CommentEntry{
		{ID: 1, Author: "avery", Body: "old comment"},
		{ID: 2, Author: "kai", Body: "new comment"},
	}
	api.events = []EventLogEntry{{ID: 9, Type: "issue.updated"}}
	api.links = []LinkEntry{{ID: 4, Type: "related"}}

	m, back := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewHelp, m.view, "leaving must restore, not close, the Help foreground")
	require.Equal(t, "Stale title", m.detail.issue.Title, "the retained pane is restored as-is until the refetch lands")

	m = drainInboxCmds(m, back)
	require.Equal(t, viewHelp, m.view, "the detail refetch must not steal the foreground from Help")
	require.Positive(t, api.detailCalls, "leaving Inbox must refetch the detail retained behind Help")
	require.Positive(t, api.commentCalls, "leaving Inbox must refetch the retained comments")
	require.Positive(t, api.eventCalls, "leaving Inbox must refetch the retained events")
	require.Positive(t, api.linkCalls, "leaving Inbox must refetch the retained links")
	require.Equal(t, "Fresh title", m.detail.issue.Title)
	require.Len(t, m.detail.comments, 2)
	require.Len(t, m.detail.events, 1)
	require.Len(t, m.detail.links, 1)

	// Exiting Help reveals the converged detail pane.
	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewDetail, m.view)
	require.Equal(t, "Fresh title", m.detail.issue.Title)
}

// TestInboxLeaveRefetchesRetainedSplitDetailBehindHelp covers the split
// flavor of the same finding: Help covers a split view whose detail
// pane stays retained behind it, so leaving the Inbox must refetch that
// pane too — it is visible again the moment Help closes.
func TestInboxLeaveRefetchesRetainedSplitDetailBehindHelp(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	stale := testIssue("aaa1", withTitle("Stale title"))
	api := &inboxTestAPI{projects: []ProjectSummary{inbox}, issues: []Issue{stale}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	m.layout = splitlayout.Split
	m.view = viewList
	m.focus = focusList
	m.detail = detailModel{issue: &stale, scopePID: 1}

	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewHelp, m.view)
	require.Equal(t, viewList, m.prevView)
	m = enterInboxForTest(t, m)

	fresh := testIssue("aaa1", withTitle("Fresh title"))
	api.detail = &IssueDetail{Issue: &fresh}

	m, back := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewHelp, m.view)
	m = drainInboxCmds(m, back)
	require.Equal(t, viewHelp, m.view, "the detail refetch must not steal the foreground from Help")
	require.Positive(t, api.detailCalls,
		"the split layout keeps the detail pane behind Help; it must refetch on leave")
	require.Equal(t, "Fresh title", m.detail.issue.Title)

	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewList, m.view)
	require.Equal(t, splitlayout.Split, m.layout)
	require.Equal(t, "Fresh title", m.detail.issue.Title)
}

func TestInboxLeaveRefetchesSplitDetailBehindProjectsAfterHelp(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	stale := testIssue("aaa1", withTitle("Stale title"))
	api := &inboxTestAPI{projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox}, issues: []Issue{stale}}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	m.layout = splitlayout.Split
	m.view = viewList
	m.focus = focusList
	m.detail = detailModel{issue: &stale, scopePID: 1}

	m, _ = updateModel(m, keyRune('P'))
	require.Equal(t, viewProjects, m.view)
	m, _ = updateModel(m, keyRune('?'))
	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewProjects, m.view)
	require.Equal(t, viewProjects, m.prevView)
	m = enterInboxForTest(t, m)

	fresh := testIssue("aaa1", withTitle("Fresh title"))
	api.detail = &IssueDetail{Issue: &fresh}
	m, back := updateModel(m, keyRune('I'))
	require.Equal(t, viewProjects, m.view)
	m = drainInboxCmds(m, back)
	require.Positive(t, api.detailCalls, "Projects exits to the split list even when prevView still names Projects")

	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewList, m.view)
	require.Equal(t, "Fresh title", m.detail.issue.Title)
}

// TestInboxResetRequiredReresolvesDesignation pins the review finding:
// sync.reset_required while the Inbox is foreground must re-resolve the
// role=inbox designation through the same lookup path the project SSE
// events use. A reset that coincides with a designation move must not
// leave the TUI listing the abandoned project, and the reset's own
// pre-swap list refetch must stay fenced out after the swap lands.
func TestInboxResetRequiredReresolvesDesignation(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	next := ProjectSummary{ID: 8, Name: "spoke-project"}
	next.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "hub-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "hub-project")
	m.width, m.height = 120, 30
	m = enterInboxForTest(t, m)
	require.Equal(t, int64(2), m.scope.projectID)

	// The designation moves; the daemon then requires a full resync.
	api.projects = []ProjectSummary{{ID: 1, Name: "hub-project"}, next}
	api.issues = []Issue{{UID: "01TEST-next", ProjectID: 8, ShortID: "next", Title: "Moved task", Status: "open"}}
	m.sseCh = nil
	m, resetCmd := updateModel(m, resetRequiredMsg{})
	require.NotNil(t, resetCmd)
	require.True(t, m.scope.inbox, "the reset itself must not drop the Inbox scope")
	require.Equal(t, viewList, m.view, "the reset must not disturb the foreground")
	require.False(t, m.cache.set, "the reset must still drop the cache")
	require.NotNil(t, m.toast)
	require.Equal(t, "resynced", m.toast.text, "the reset toast semantics must be preserved")

	// The reset batch must carry a designation lookup alongside the
	// pre-swap list refetch. Hold both replies so the fencing order is
	// explicit: the lookup lands before the slow refetch reply.
	var loaded *inboxProjectLoadedMsg
	var staleRefetched *refetchedMsg
	for _, msg := range inboxCmdMsgs(resetCmd) {
		switch msg := msg.(type) {
		case inboxProjectLoadedMsg:
			loaded = &msg
		case refetchedMsg:
			staleRefetched = &msg
		}
	}
	require.NotNil(t, loaded, "a reset on the Inbox scope must re-resolve the designation")
	require.NotNil(t, staleRefetched, "the reset must still refetch the current inbox list")
	require.True(t, loaded.found)
	require.Equal(t, int64(8), loaded.project.ID)

	// The lookup lands first and moves the Inbox to the new designation.
	m, swapFetch := updateModel(m, *loaded)
	require.True(t, m.scope.inbox)
	require.Equal(t, int64(8), m.scope.projectID)
	require.NotNil(t, swapFetch, "the moved designation must trigger a fresh list fetch")

	// The pre-swap refetch reply lands after the swap and must be
	// fenced out by its stale inboxVisit, not overwrite the moved list.
	m, _ = updateModel(m, *staleRefetched)
	require.Empty(t, m.list.issues,
		"a pre-swap reset refetch landing after the designation move must be dropped")

	m, _ = updateModel(m, swapFetch())
	require.Equal(t, int64(8), api.listProjectID)
	require.Len(t, m.list.issues, 1)
	require.Equal(t, "Moved task", m.list.issues[0].Title)
	require.True(t, m.cache.set)
}

// TestInboxResetRequiredUnderHelpDefersUntilExit pins the foreground
// rule for the reset path: a sync.reset_required landing while Help
// covers the Inbox must defer the designation re-resolve to the
// global-view exit path instead of resolving — and possibly leaving —
// the Inbox over a view the user is reading.
func TestInboxResetRequiredUnderHelpDefersUntilExit(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	m = enterInboxForTest(t, m)
	m, _ = updateModel(m, keyRune('?'))
	require.Equal(t, viewHelp, m.view)

	// The designation is removed; the daemon then requires a full resync
	// while Help still covers the Inbox.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	api.issues = []Issue{{UID: "01TEST-old", ProjectID: 1, ShortID: "old", Title: "Prior work", Status: "open"}}
	m.sseCh = nil
	m, resetCmd := updateModel(m, resetRequiredMsg{})
	require.NotNil(t, resetCmd)
	require.False(t, m.cache.set, "the reset must still drop the cache while a view covers the Inbox")
	require.True(t, m.scope.inbox, "the covered Inbox scope must not change under the reset")
	require.Equal(t, viewHelp, m.view, "the reset must not close the foreground view")
	require.True(t, m.inboxReresolvePending,
		"a reset under a covering view must defer the designation re-resolve to the exit path")

	for _, msg := range inboxCmdMsgs(resetCmd) {
		if _, ok := msg.(inboxProjectLoadedMsg); ok {
			t.Fatal("no designation lookup may resolve while a global view is foreground")
		}
	}

	// Exiting Help returns to the Inbox list and re-resolves; the
	// removal then leaves the Inbox with the usual notice and restore.
	m, lookup := updateModel(m, keyRune('?'))
	require.Equal(t, viewList, m.view)
	require.NotNil(t, lookup, "the exit path must re-resolve the deferred designation")
	m, leaveCmd := deliverInboxLifecycleEvent(m, lookup)
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(1), m.scope.projectID)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")

	m = runInboxRestoreCmd(m, leaveCmd)
	require.Equal(t, int64(1), api.listProjectID, "the restored project's list is fetched")
	require.Len(t, m.list.issues, 1)
	require.Equal(t, "Prior work", m.list.issues[0].Title)
}

func TestInboxDeferredReresolveWhenHelpReturnsToDetail(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues:   []Issue{{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"}},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	m = enterInboxForTest(t, m)
	m.view, m.prevView = viewHelp, viewDetail

	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	m.sseCh = nil
	out, _ := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 2})
	m = out.(Model)
	require.True(t, m.inboxReresolvePending)

	m, lookup := updateModel(m, keyRune('?'))
	require.Equal(t, viewDetail, m.view)
	require.NotNil(t, lookup, "returning to an Inbox detail pane must resolve deferred designation changes")

	m, _ = deliverInboxLifecycleEvent(m, lookup)
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(1), m.scope.projectID)
}

// TestInboxPendingEntryRestartsWhenDesignationCleared pins the review
// finding: a project.metadata_updated that clears the role=inbox
// designation can land after the I-key lookup's ListProjects snapshot
// but before its reply. The frame used to be ignored because the entry
// had not applied yet (m.scope.inbox still false), so the stale reply
// entered an Inbox scoped to the de-designated project. The frame must
// restart the lookup instead: the bumped attempt fences the stale
// reply, and the fresh reply applies the post-event designation —
// here, none.
func TestInboxPendingEntryRestartsWhenDesignationCleared(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	// I resolves the designation; the ListProjects snapshot — role still
	// on project 2 — is in flight.
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	staleReply := lookup()

	// The daemon clears the role; the SSE frame lands after the snapshot
	// but before the reply.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd,
		"a designation-clearing frame during a pending entry must restart the lookup")
	restart := inFlightInboxLookup(t, eventCmd)

	// The stale reply from the earlier attempt lands first.
	m, staleCmd := updateModel(m, staleReply)
	require.False(t, m.scope.inbox, "the stale pre-event reply must not enter the Inbox")
	require.Equal(t, int64(1), m.scope.projectID, "the scope must stay on the pre-entry project")
	require.Nil(t, staleCmd)

	// The restarted lookup reads the post-event designation: none.
	m, _ = updateModel(m, restart)
	require.False(t, m.scope.inbox, "with the role cleared the entry must not land")
	require.Nil(t, m.inboxReturn)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")
}

// TestInboxPendingEntryRestartsWhenDesignatedProjectRemoved covers the
// project.removed variant of the pending-entry race: the designated
// project is archived after the I-key snapshot but before its reply.
// The frame must restart the lookup so the stale reply is fenced and
// the entry resolves against the archive.
func TestInboxPendingEntryRestartsWhenDesignatedProjectRemoved(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	staleReply := lookup()

	// The daemon archives the designated project (ListProjects excludes
	// archived rows) and the project.removed frame lands mid-flight.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.removed", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd,
		"a removed designated project during a pending entry must restart the lookup")
	restart := inFlightInboxLookup(t, eventCmd)

	m, staleCmd := updateModel(m, staleReply)
	require.False(t, m.scope.inbox, "the stale pre-archive reply must not enter the Inbox")
	require.Equal(t, int64(1), m.scope.projectID)
	require.Nil(t, staleCmd)

	m, _ = updateModel(m, restart)
	require.False(t, m.scope.inbox)
	require.Nil(t, m.inboxReturn)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")
}

// TestInboxPendingEntryLandsMovedDesignation proves the restart is not
// only a fence: when the role moves to a different project between the
// I-key snapshot and its reply, the restarted lookup enters the new
// designated project with its issues, while the stale reply — scoped to
// the abandoned project — is dropped.
func TestInboxPendingEntryLandsMovedDesignation(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "hub-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "hub-project")
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	staleReply := lookup()

	// The role moves to a different project before the reply lands.
	moved := ProjectSummary{ID: 3, Name: "spoke-project"}
	moved.Metadata.Role = jsontext.Value(`"inbox"`)
	api.projects = []ProjectSummary{{ID: 1, Name: "hub-project"}, moved}
	api.issues = []Issue{
		{UID: "01TEST-moved", ProjectID: 3, ShortID: "moved", Title: "Moved task", Status: "open"},
	}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.metadata_updated", projectID: 3})
	m = out.(Model)
	require.NotNil(t, eventCmd,
		"a designation-moving frame during a pending entry must restart the lookup")
	restart := inFlightInboxLookup(t, eventCmd)

	m, staleCmd := updateModel(m, staleReply)
	require.False(t, m.scope.inbox, "the stale reply scoped to the old project must not enter")
	require.Nil(t, staleCmd)

	m, fetch := updateModel(m, restart)
	require.True(t, m.scope.inbox, "the restarted lookup must land the moved designation")
	require.Equal(t, int64(3), m.scope.projectID)
	require.Equal(t, "spoke-project", m.scope.projectName)
	require.NotNil(t, fetch, "the moved designation must trigger a fresh list fetch")
	m, _ = updateModel(m, fetch())
	require.Equal(t, int64(3), api.listProjectID)
	require.Len(t, m.list.issues, 1)
	require.Equal(t, "Moved task", m.list.issues[0].Title)
}

// TestInboxPendingEntryRestartsOnResetRequired covers the same race for
// sync.reset_required: a purge landing during the pending entry interval
// can coincide with a designation clear, and the in-flight snapshot may
// predate the reset. The reset must restart the lookup so the stale
// reply is fenced, without silently cancelling the entry.
func TestInboxPendingEntryRestartsOnResetRequired(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	staleReply := lookup()

	// The purge clears the designation; the reset frame lands before the
	// pending reply.
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}}
	m.sseCh = nil
	m, resetCmd := updateModel(m, resetRequiredMsg{})
	require.NotNil(t, resetCmd)
	require.False(t, m.scope.inbox, "the reset must not drop the pre-entry scope")
	require.True(t, m.inboxPending, "the reset must not silently cancel the pending entry")

	var restart *inboxProjectLoadedMsg
	for _, msg := range inboxCmdMsgs(resetCmd) {
		if loaded, ok := msg.(inboxProjectLoadedMsg); ok {
			restart = &loaded
		}
	}
	require.NotNil(t, restart,
		"a reset during a pending entry must restart the designation lookup")

	m, staleCmd := updateModel(m, staleReply)
	require.False(t, m.scope.inbox, "the stale pre-reset reply must not enter the Inbox")
	require.Equal(t, int64(1), m.scope.projectID)
	require.Nil(t, staleCmd)

	m, _ = updateModel(m, *restart)
	require.False(t, m.scope.inbox)
	require.Nil(t, m.inboxReturn)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "No Inbox project designated")
}

// TestInboxPendingEntryUnaffectedByIssueEvent pins the preserved
// behavior that bounds the restart rule: only designation-affecting
// frames restart a pending lookup. An ordinary issue event during the
// pending window must not fence the entry — the I-key still lands in
// the designated project.
func TestInboxPendingEntryUnaffectedByIssueEvent(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)

	m.sseCh = nil
	out, _ := m.handleEventReceived(eventReceivedMsg{eventType: "issue.created", projectID: 1})
	m = out.(Model)

	m, fetch := updateModel(m, lookup())
	require.True(t, m.scope.inbox, "an issue event during the pending window must not fence the entry")
	require.Equal(t, int64(2), m.scope.projectID)
	require.NotNil(t, fetch)
}

// TestInboxPendingEntrySurvivesCursorAndScrollKeys pins the review
// finding: cursor and scroll keys (j, k, arrows) pressed while an
// I-key entry lookup is in flight are harmless navigation on the view
// the user is reading. They must not cancel the pending entry —
// routeTopLevel used to clear inboxPending on every non-I keypress,
// and on views whose router consumes cursor keys (projects table,
// daemons, federation, credentials) the cleared model is the one that
// gets returned, so the lookup reply was dropped and on a slow daemon
// the Inbox appeared broken after an innocent scroll. The late reply
// must still open the designated Inbox.
func TestInboxPendingEntrySurvivesCursorAndScrollKeys(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30
	// The user browses the projects table when the entry starts: its
	// router consumes cursor keys, which is where the clearing used to
	// take effect.
	m.view = viewProjects
	injectProjects(&m,
		mockProject{ID: 1, Name: "spoke-project", Ident: "spoke"},
		mockProject{ID: 2, Name: "example-project", Ident: "example"},
	)

	// I starts the entry lookup; the ListProjects reply is in flight.
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	reply := lookup()

	// Harmless cursor/scroll keys while the lookup is in flight: each
	// must leave the pending entry alone and keep the pre-entry view.
	for _, key := range []tea.KeyPressMsg{
		keyRune('j'),
		keyRune('k'),
		{Code: tea.KeyDown},
		{Code: tea.KeyUp},
	} {
		m, _ = updateModel(m, key)
		require.True(t, m.inboxPending,
			"cursor key %q during a pending entry must not cancel it", key.String())
		require.False(t, m.scope.inbox,
			"cursor key %q must not land the entry early", key.String())
		require.Nil(t, m.inboxReturn)
	}

	// The eventual reply opens the designated Inbox despite the scrolls.
	m, fetch := updateModel(m, reply)
	require.True(t, m.scope.inbox, "the lookup reply must open the Inbox after cursor keys")
	require.Equal(t, int64(2), m.scope.projectID)
	require.Equal(t, "example-project", m.scope.projectName)
	require.NotNil(t, m.inboxReturn, "the entry must capture the pre-entry view for esc")
	require.NotNil(t, fetch)
	m, _ = updateModel(m, fetch())
	require.Equal(t, int64(2), api.listProjectID)
	require.Len(t, m.list.issues, 1)
	require.Equal(t, "Capture task", m.list.issues[0].Title)
}

// TestInboxPendingEntryCancelledByForegroundKeys pins the contract the
// cursor-key survival must not break: foreground-changing keys (P and
// esc) still cancel a pending entry, and the late lookup reply must not
// switch the new foreground out from under the user.
func TestInboxPendingEntryCancelledByForegroundKeys(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	// P during the pending entry switches to Projects and cancels it.
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	staleReply := lookup()

	m, _ = updateModel(m, keyRune('P'))
	require.False(t, m.inboxPending, "P must cancel the pending entry")
	require.Equal(t, viewProjects, m.view)

	// The late reply must not yank the Projects view into the Inbox.
	m, cmd := updateModel(m, staleReply)
	require.False(t, m.scope.inbox, "a cancelled entry's late reply must not open the Inbox")
	require.Equal(t, viewProjects, m.view, "the late reply must not change the foreground")
	require.Nil(t, m.inboxReturn)
	require.Nil(t, cmd)

	// Esc cancels a pending entry from the Projects view as well.
	m, lookup = updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	staleReply = lookup()

	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.False(t, m.inboxPending, "esc must cancel the pending entry")

	m, cmd = updateModel(m, staleReply)
	require.False(t, m.scope.inbox, "a cancelled entry's late reply must not open the Inbox")
	require.Nil(t, m.inboxReturn)
	require.Nil(t, cmd)
}

// TestInboxPendingEntryRestartsWhenArchivedInboxRestored pins the
// project.restored variant of the pending-entry race: archiving the
// designated project keeps its role=inbox metadata, so restoring it
// re-enters the designation scan without any metadata write. The
// I-key snapshot — taken while the project was still archived and
// ListProjects reported no Inbox — may predate the restore frame. The
// frame must restart the lookup: the bumped attempt fences the stale
// reply, and the fresh reply opens the restored designated Inbox
// instead of reporting "No Inbox project designated".
func TestInboxPendingEntryRestartsWhenArchivedInboxRestored(t *testing.T) {
	useNoColor(t)
	// Project 2 is archived: it holds role=inbox metadata but ListProjects
	// excludes archived rows, so the snapshot reports no designation.
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 1, Name: "spoke-project"}},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "spoke-project")
	m.width, m.height = 120, 30

	// I starts the entry; the snapshot showing no Inbox is in flight.
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	require.True(t, m.inboxPending)
	staleReply := lookup()

	// The daemon restores project 2. Restore only clears deleted_at, so
	// the role=inbox metadata survives and the frame lands mid-flight.
	restored := ProjectSummary{ID: 2, Name: "example-project"}
	restored.Metadata.Role = jsontext.Value(`"inbox"`)
	api.projects = []ProjectSummary{{ID: 1, Name: "spoke-project"}, restored}
	api.issues = []Issue{
		{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
	}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.restored", projectID: 2})
	m = out.(Model)
	require.NotNil(t, eventCmd,
		"a project.restored frame during a pending entry must restart the lookup")
	restart := inFlightInboxLookup(t, eventCmd)

	// The stale reply from the earlier attempt lands first and is fenced.
	m, staleCmd := updateModel(m, staleReply)
	require.False(t, m.scope.inbox, "the stale pre-restore reply must not close the entry")
	require.Equal(t, int64(1), m.scope.projectID, "the scope must stay on the pre-entry project")
	require.Nil(t, staleCmd)

	// The restarted lookup reads the post-restore designation and enters.
	m, fetch := updateModel(m, restart)
	require.True(t, m.scope.inbox, "the restarted lookup must land the restored designation")
	require.Equal(t, int64(2), m.scope.projectID)
	require.Equal(t, "example-project", m.scope.projectName)
	require.NotNil(t, fetch, "the restored designation must trigger a fresh list fetch")
	m, _ = updateModel(m, fetch())
	require.Equal(t, int64(2), api.listProjectID)
	require.Len(t, m.list.issues, 1)
	require.Equal(t, "Capture task", m.list.issues[0].Title)
}

// TestInboxActiveInboxReresolvesWhenArchivedInboxRestored pins the
// active-Inbox rule for project.restored: an archived project can
// reappear carrying role=inbox metadata, which changes the designation
// scan's outcome without any metadata_updated frame. The active Inbox
// must re-resolve through a fresh lookup and follow the scan result.
func TestInboxActiveInboxReresolvesWhenArchivedInboxRestored(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "hub-project")
	m.width, m.height = 120, 30
	m = enterInboxForTest(t, m)
	returnState := m.inboxReturn
	require.NotNil(t, returnState)

	// The daemon restores archived project 5, which still carries
	// role=inbox metadata from before its archive. The designation scan
	// now yields project 5 first.
	restored := ProjectSummary{ID: 5, Name: "spoke-project"}
	restored.Metadata.Role = jsontext.Value(`"inbox"`)
	api.projects = []ProjectSummary{restored, inbox}
	api.issues = []Issue{
		{UID: "01TEST-restored", ProjectID: 5, ShortID: "restored", Title: "Restored task", Status: "open"},
	}
	m.sseCh = nil
	out, eventCmd := m.handleEventReceived(eventReceivedMsg{eventType: "project.restored", projectID: 5})
	m = out.(Model)
	require.NotNil(t, eventCmd,
		"a project.restored frame must trigger a fresh designation lookup")
	m, fetch := deliverInboxLifecycleEvent(m, eventCmd)

	require.True(t, m.scope.inbox, "the restored designation keeps the Inbox open")
	require.Equal(t, int64(5), m.scope.projectID,
		"the Inbox must follow the restored project's designation")
	require.Equal(t, "spoke-project", m.scope.projectName)
	require.Same(t, returnState, m.inboxReturn, "the original return state survives the re-resolve")
	require.NotNil(t, fetch, "the moved designation must trigger a fresh list fetch")
	m, _ = updateModel(m, fetch())
	require.Equal(t, int64(5), api.listProjectID)
	require.Equal(t, "Restored task", m.list.issues[0].Title)
}

// TestInboxRestoreDuringProjectsDefersUntilExit pins the foreground
// rule for project.restored: a restore frame landing while a global
// view covers the Inbox must defer like the other designation-affecting
// frames, and resolve through the exit path without disturbing the
// covered foreground.
func TestInboxRestoreDuringProjectsDefersUntilExit(t *testing.T) {
	useNoColor(t)
	inbox := ProjectSummary{ID: 2, Name: "example-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(1, "hub-project")
	m.width, m.height = 120, 30
	m = enterInboxForTest(t, m)
	returnState := m.inboxReturn
	m, _ = updateModel(m, keyRune('P'))
	require.Equal(t, viewProjects, m.view)

	restored := ProjectSummary{ID: 5, Name: "spoke-project"}
	restored.Metadata.Role = jsontext.Value(`"inbox"`)
	api.projects = []ProjectSummary{restored, inbox}
	api.issues = []Issue{
		{UID: "01TEST-restored", ProjectID: 5, ShortID: "restored", Title: "Restored task", Status: "open"},
	}
	m.sseCh = nil
	out, _ := m.handleEventReceived(eventReceivedMsg{eventType: "project.restored", projectID: 5})
	m = out.(Model)
	require.True(t, m.inboxReresolvePending,
		"a restore frame under a global view must defer the re-resolve")
	require.Equal(t, viewProjects, m.view, "the restore must not close the foreground view")
	require.Same(t, returnState, m.inboxReturn)
	require.Nil(t, m.toast)

	// Exiting Projects returns to the Inbox list and re-resolves.
	m, resume := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, viewList, m.view)
	require.NotNil(t, resume, "returning to the Inbox must re-resolve the deferred restore")
	m, _ = deliverInboxLifecycleEvent(m, resume)
	require.True(t, m.scope.inbox, "the restored designation keeps the Inbox open")
	require.Equal(t, int64(5), m.scope.projectID)
	require.Same(t, returnState, m.inboxReturn)
}

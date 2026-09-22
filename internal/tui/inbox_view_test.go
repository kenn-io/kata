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
	var issues []Issue
	for _, issue := range a.issues {
		if filter.Status == "" || issue.Status == filter.Status {
			issues = append(issues, issue)
		}
	}
	return issues, nil
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

// If the project disappears after lookup, show the fetch error and let Esc
// restore the previous list and filters.
func TestInboxMissingProjectAllowsEscape(t *testing.T) {
	useNoColor(t)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			respondJSON(t, w, map[string]any{"projects": []map[string]any{
				{"id": 2, "name": "example-project", "metadata": map[string]any{"role": "inbox"}},
			}})
		case "/api/v1/projects/2/issues":
			require.Equal(t, "open", r.URL.Query().Get("status"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			respondJSON(t, w, map[string]any{"error": map[string]string{
				"code": "project_not_found", "message": "project not found",
			}})
		case "/api/v1/projects/1/issues":
			respondJSON(t, w, map[string]any{"issues": []Issue{
				{UID: "01TEST-prior", ProjectID: 1, Title: "Prior task", Status: "open"},
			}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	m := newTestModel()
	m.api = client
	m.scope = homedScope(1, "spoke-project")
	m.list.filter.Search = "Prior"
	m.width, m.height = 100, 30
	m = enterInboxForTest(t, m)
	require.Contains(t, stripANSI(m.viewContent()), "project not found")

	m, restore := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	m = drainInboxCmds(m, restore)
	require.Equal(t, int64(1), m.scope.projectID)
	require.Equal(t, "Prior", m.list.filter.Search)
	require.Contains(t, stripANSI(m.viewContent()), "Prior task")
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
	project := ProjectSummary{ID: 7, Name: "example-project"}
	project.Metadata.Role = jsontext.Value(`"inbox"`)
	m.api = &inboxTestAPI{projects: []ProjectSummary{project}}
	m = enterInboxForTest(t, m)
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

// A list reply from before Inbox entry must not overwrite the refreshed
// list or cache after leaving Inbox.
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

// Esc cancels an active priority prompt first. A second Esc leaves Inbox.
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

// Leaving Inbox refreshes the restored issue, comments, events, and links.
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

// Leaving Inbox refreshes the detail pane visible beside the restored list.
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

// runInboxRestoreCmd feeds a command chain back into the model.
// Toast-expiry commands use a 3s tea.Tick timer, so commands run
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

// Inbox always shows open tasks; the status key gives a hint instead
// of switching to closed or all tasks.
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

// Inbox filters offer Owner, Search, and Labels while keeping status open.
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

// Leaving Inbox restores Help and refreshes its hidden list, so closing
// Help reveals current tasks without waiting for another event.
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

// Leaving Inbox refreshes the list behind each restored full-screen view.
// Credentials closes before entry, so it returns through the ordinary list path.
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

// Leaving Inbox refreshes the detail retained behind Help without closing
// Help. Closing Help then reveals the current issue and activity.
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

// Leaving Inbox refreshes the split detail pane retained behind Help.
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

// An issue event does not cancel a pending Inbox entry.
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

// Cursor and scroll keys keep a pending Inbox entry active on every view.
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

// Changing views or pressing Esc cancels a pending Inbox entry.
// A late reply must not replace the new foreground.
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

// Inbox keeps the project chosen on entry. Events still refresh its tasks;
// leaving and reopening Inbox picks up the current designation.
func TestInboxDesignationChangesApplyOnNextVisit(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"project event", eventReceivedMsg{eventType: "project.metadata_updated", projectID: 7}},
		{"stream reset", resetRequiredMsg{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := ProjectSummary{ID: 7, Name: "example-project"}
			first.Metadata.Role = jsontext.Value(`"inbox"`)
			second := ProjectSummary{ID: 8, Name: "spoke-project"}
			second.Metadata.Role = jsontext.Value(`"inbox"`)
			api := &inboxTestAPI{projects: []ProjectSummary{first}}
			m := newTestModel()
			m.api = api
			m.scope = homedScope(1, "hub-project")
			m.sseCh = nil
			m = enterInboxForTest(t, m)

			api.projects = []ProjectSummary{second}
			api.issues = []Issue{{UID: "01TEST-fresh", ProjectID: 7, Title: "Updated capture", Status: "open"}}
			m, refresh := updateModel(m, tc.msg)
			m = runInboxRestoreCmd(m, refresh)
			require.Equal(t, int64(7), m.scope.projectID)
			require.Equal(t, int64(7), api.listProjectID)
			require.Equal(t, "open", api.listStatus)
			require.Len(t, m.list.issues, 1)
			require.Equal(t, "Updated capture", m.list.issues[0].Title)

			m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
			require.Equal(t, int64(1), m.scope.projectID)
			api.issues = []Issue{{UID: "01TEST-next", ProjectID: 8, Title: "New Inbox capture", Status: "open"}}
			m = enterInboxForTest(t, m)
			require.Equal(t, int64(8), api.listProjectID)
			require.Equal(t, "New Inbox capture", m.list.issues[0].Title)
		})
	}
}

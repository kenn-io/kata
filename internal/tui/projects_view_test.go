package tui

import (
	"encoding/json/jsontext"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectsView_RendersWithoutPanic confirms the view renders a
// non-empty frame for a model in viewProjects state, even with no
// projects loaded yet. Required for boot landing where the fetch is
// still in flight.
func TestProjectsView_RendersWithoutPanic(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView()
	m.width = 80 // narrower than the 120 default

	out := m.viewContent()
	if out == "" {
		t.Fatal("viewProjects must render a non-empty frame")
	}
	if !strings.Contains(out, "projects") {
		t.Errorf("expected 'projects' in viewProjects output:\n%s", out)
	}
}

// TestProjectsRows_SentinelSumsAndPinsFirst pins spec §1.6: the All-
// projects sentinel row's Open/Closed are the sum of per-row counts and
// LastEventAt is the row-max. The sentinel is always at index 0.
func TestProjectsRows_SentinelSumsAndPinsFirst(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC) // newer
	byID := map[int64]string{1: "kata", 2: "roborev", 3: "msgvault"}
	idents := map[int64]string{1: "github.com/wesm/kata", 2: "...", 3: "..."}
	stats := map[int64]ProjectStatsSummary{
		1: {Open: 5, Closed: 2, LastEventAt: &t2},
		2: {Open: 3, Closed: 1, LastEventAt: &t1},
		3: {Open: 0, Closed: 0, LastEventAt: nil},
	}
	rows := projectsRows(byID, idents, stats)
	require.Len(t, rows, 4) // sentinel + 3 projects
	assert.True(t, rows[0].sentinel, "row 0 must be the sentinel")
	assert.Equal(t, 8, rows[0].stats.Open, "sentinel open = 5+3+0")
	assert.Equal(t, 3, rows[0].stats.Closed, "sentinel closed = 2+1+0")
	require.NotNil(t, rows[0].stats.LastEventAt)
	assert.True(t, rows[0].stats.LastEventAt.Equal(t2), "sentinel last = max(t1, t2) = t2")
}

// TestProjectsRows_SortByLastEventDesc pins spec §5.3: rows after the
// sentinel are sorted by last_event_at desc with name asc as the
// tiebreak. A row with no events sinks to the bottom.
func TestProjectsRows_SortByLastEventDesc(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	byID := map[int64]string{1: "older", 2: "newer", 3: "noevents"}
	idents := map[int64]string{1: "...", 2: "...", 3: "..."}
	stats := map[int64]ProjectStatsSummary{
		1: {LastEventAt: &t1},
		2: {LastEventAt: &t2},
		3: {LastEventAt: nil},
	}
	rows := projectsRows(byID, idents, stats)
	assert.True(t, rows[0].sentinel)
	assert.Equal(t, "newer", rows[1].name)
	assert.Equal(t, "older", rows[2].name)
	assert.Equal(t, "noevents", rows[3].name)
}

// TestProjectsRows_StableTiebreakerOnEqualNamesAndTimes pins that two
// projects with identical last_event_at and equal-folded names sort
// deterministically by projectID. Without this, render and key-handling
// could see different orderings between calls (Go map iteration is
// intentionally randomized), so Enter on the highlighted row would
// occasionally select the wrong project.
func TestProjectsRows_StableTiebreakerOnEqualNamesAndTimes(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	byID := map[int64]string{
		100: "Kata",
		50:  "kata", // same fold
	}
	idents := map[int64]string{100: "github.com/wesm/kata-100", 50: "github.com/wesm/kata-50"}
	stats := map[int64]ProjectStatsSummary{
		100: {LastEventAt: &t1},
		50:  {LastEventAt: &t1},
	}
	for range 50 {
		rows := projectsRows(byID, idents, stats)
		require.Len(t, rows, 3)
		assert.Equal(t, int64(50), rows[1].projectID, "lower projectID first on tie")
		assert.Equal(t, int64(100), rows[2].projectID)
	}
}

// TestProjectsView_RendersTable confirms the table renders with the
// expected column headers and row content for a fixture model. Wide
// terminal so all columns fit.
func TestProjectsView_RendersTable(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	t1 := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m := setupProjectsView(
		mockProject{ID: 1, Name: "kata", Ident: "github.com/wesm/kata", Stats: ProjectStatsSummary{Open: 12, Closed: 3, LastEventAt: &t1}},
		mockProject{ID: 2, Name: "roborev", Ident: "github.com/wesm/roborev", Stats: ProjectStatsSummary{Open: 7, Closed: 2, LastEventAt: &t1}},
	)

	out := m.viewContent()
	for _, want := range []string{
		"kata / projects", "Project", "Open", "Closed", "Total", "Updated",
		"All projects", "kata", "roborev",
	} {
		assert.Contains(t, out, want, "missing %q in viewProjects output", want)
	}
}

func TestProjectsView_FooterUsesAdaptiveHelpTable(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(mockProject{ID: 1, Name: "alpha", Ident: "..."})

	out := stripANSI(m.viewContent())

	assert.Contains(t, out, "▕")
	assert.Contains(t, out, "F federation")
	assert.NotContains(t, out, "[F] federation")
}

func TestProjectsView_FTransitionsToFederationWithHighlightedProjectSelected(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 11, Name: "alpha-project", Ident: "..."},
		mockProject{ID: 22, Name: "beta-project", Ident: "..."},
	)
	m.scope = homedScope(99, "previous-project")
	m.projectsCursor = 2

	out, cmd := updateModel(m, keyRune('F'))

	assert.Equal(t, viewFederation, out.view)
	require.NotNil(t, cmd)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "selected project: beta-project")

	out, cmd = out.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	// The local-project step is shown with the highlighted project pre-cursored
	// (never skipped); one Enter proceeds to hub selection with it adopted.
	assert.Equal(t, federationModeSelectLocalProject, out.federation.mode)
	rows := federationLocalProjectRows(out)
	require.Greater(t, len(rows), out.federation.localProjectCursor)
	require.False(t, rows[out.federation.localProjectCursor].createReplica)
	assert.Equal(t, "beta-project", rows[out.federation.localProjectCursor].project.Name)

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federation.mode)
	assert.Equal(t, int64(22), out.federation.draft.SpokeProjectID)
	assert.Equal(t, "beta-project", out.federation.draft.SpokeProjectName)
}

func TestProjectsView_FTransitionSelectedProjectIgnoresStaleFederationDraft(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 11, Name: "alpha-project", Ident: "..."},
		mockProject{ID: 22, Name: "beta-project", Ident: "..."},
	)
	m.projectsCursor = 2
	m.federation.draft.SpokeProjectName = "stale-project"

	out, cmd := updateModel(m, keyRune('F'))

	assert.Equal(t, viewFederation, out.view)
	require.NotNil(t, cmd)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "selected project: beta-project")
	assert.NotContains(t, rendered, "selected project: stale-project")
}

func TestProjectsView_FFromAllProjectsHasNoSelectedFederationProject(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(mockProject{ID: 11, Name: "alpha-project", Ident: "..."})
	m.scope = homedScope(99, "previous-project")
	m.projectsCursor = 0

	out, cmd := updateModel(m, keyRune('F'))

	assert.Equal(t, viewFederation, out.view)
	require.NotNil(t, cmd)
	assert.Contains(t, stripANSI(renderFederation(out)), "selected project: none")

	out, cmd = out.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectLocalProject, out.federation.mode)
}

// TestProjectsView_ViewportClipsRowsToHeight pins that with many
// projects and a small terminal, the footer + key-hint line stay on
// screen. Without clipping, every row renders and the chrome falls
// off the bottom — the user can't see [↑/↓ k/j] move etc.
func TestProjectsView_ViewportClipsRowsToHeight(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView()
	m.height = 14 // chrome=8 + ~5 row slots
	for i := int64(1); i <= 20; i++ {
		injectProjects(&m, mockProject{
			ID:    i,
			Name:  "proj" + strconv.FormatInt(i, 10),
			Ident: "github.com/wesm/proj" + strconv.FormatInt(i, 10),
		})
	}
	m.projectsCursor = 10

	// Strip style SGRs: v2 styles always emit them, and the footer
	// assertions match across key/desc style boundaries.
	out := stripANSI(m.viewContent())
	lines := strings.Split(out, "\n")
	assert.LessOrEqual(t, len(lines), m.height, "render must fit within m.height")
	assert.Contains(t, out, "F federation", "footer help table must remain visible")
	assert.Contains(t, out, "▕", "footer help table must use adaptive separators")
	assert.Contains(t, out, "All projects", "sentinel must remain visible")
}

// TestProjectsView_DashWhenNoEvents pins spec §6.1: a row with
// LastEventAt=nil renders "—" in the Updated column.
func TestProjectsView_DashWhenNoEvents(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 1, Name: "fresh", Ident: "github.com/wesm/fresh"},
	)
	out := m.viewContent()
	assert.Contains(t, out, "—", "nil LastEventAt must render as em-dash")
}

// TestProjectsView_ProjectFooterOnHighlight pins spec §5.1:
// highlighting a real project renders its name beneath the table;
// highlighting the sentinel renders the description.
func TestProjectsView_ProjectFooterOnHighlight(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 1, Name: "kata", Ident: "github.com/wesm/kata"},
	)

	m.projectsCursor = 0 // sentinel row
	out := m.viewContent()
	assert.Contains(t, out, "issue queue across every registered project")

	m.projectsCursor = 1 // kata row
	out = m.viewContent()
	assert.Contains(t, out, "project: kata")
}

// TestProjectsView_JKMoveCursor pins basic vertical navigation. Cursor
// is clamped at both ends; j moves down, k moves up.
func TestProjectsView_JKMoveCursor(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 1, Name: "a", Ident: "..."},
		mockProject{ID: 2, Name: "b", Ident: "..."},
		mockProject{ID: 3, Name: "c", Ident: "..."},
	)
	m.projectsCursor = 0

	out, _ := m.routeProjectsViewKey(keyRune('j'))
	assert.Equal(t, 1, out.projectsCursor, "j → cursor 1")

	out, _ = out.routeProjectsViewKey(keyRune('j'))
	out, _ = out.routeProjectsViewKey(keyRune('j'))
	out, _ = out.routeProjectsViewKey(keyRune('j'))
	assert.Equal(t, 3, out.projectsCursor, "j past end is clamped")

	out, _ = out.routeProjectsViewKey(keyRune('k'))
	assert.Equal(t, 2, out.projectsCursor, "k → cursor 2")
}

// TestProjectsView_EnterOnProjectTransitions pins spec §5.4: Enter on
// a real project sets scope to that project and transitions to viewList
// with a fresh fetch dispatched.
func TestProjectsView_EnterOnProjectTransitions(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 7, Name: "kata", Ident: "..."},
		mockProject{ID: 9, Name: "roborev", Ident: "..."},
	)
	// Prime the cache so isStale() can register invalidation
	// (isStale requires cache.set==true; see cache.go:51-52).
	m.cache.put(cacheKey{allProjects: true}, []Issue{{UID: "01TEST-aaa1", ShortID: "aaa1"}})
	// Cursor on the first real project (sentinel + sorted rows; see
	// projectsRows for ordering — alpha tiebreak means 'kata' first).
	m.projectsCursor = 1

	out, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Equal(t, viewList, out.view)
	assert.False(t, out.scope.allProjects, "concrete project, not all-projects")
	assert.Equal(t, int64(7), out.scope.projectID)
	assert.Equal(t, "kata", out.scope.projectName)
	require.NotNil(t, cmd, "must dispatch a fetch")
	assert.Equal(t, "kata", out.scope.homeProjectName, "home project tracked for R-toggle")
	assert.Equal(t, int64(7), out.scope.homeProjectID)
	assert.True(t, out.cache.isStale(), "issue cache must be invalidated on scope change")
}

// TestProjectsView_EnterOnSentinelTransitions pins that Enter on the
// All-projects row sets allProjects=true and transitions to viewList.
func TestProjectsView_EnterOnSentinelTransitions(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 1, Name: "a", Ident: "..."},
	)
	// Prime the cache so isStale() can register invalidation
	// (isStale requires cache.set==true; see cache.go:51-52).
	m.cache.put(cacheKey{projectID: 1}, []Issue{{UID: "01TEST-aaa1", ShortID: "aaa1"}})
	m.projectsCursor = 0 // sentinel

	out, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Equal(t, viewList, out.view)
	assert.True(t, out.scope.allProjects)
	assert.Zero(t, out.scope.projectID)
	require.NotNil(t, cmd)
	assert.True(t, out.cache.isStale(), "issue cache must be invalidated on scope change")
}

// TestProjectsView_EnterOnCurrentScopeIsIdempotent pins the idempotent
// re-selection contract: re-selecting the row that matches the active
// scope just returns to viewList — no cache invalidation, no refetch.
func TestProjectsView_EnterOnCurrentScopeIsIdempotent(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView(
		mockProject{ID: 7, Name: "kata", Ident: "..."},
	)
	m.scope = homedScope(7, "kata")
	m.projectsCursor = 1 // the kata row

	out, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Equal(t, viewList, out.view, "transitions to viewList")
	assert.Nil(t, cmd, "no refetch on idempotent re-select")
	assert.False(t, out.cache.isStale(), "cache untouched on idempotent re-select")
}

// TestProjectsView_EscReturnsToPriorList pins spec §1.4: Esc from
// viewProjects returns to viewList without a refetch when scope is set
// (the user came from a list via P).
func TestProjectsView_EscReturnsToPriorList(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView()
	m.scope = homedScope(7, "kata")

	out, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.Equal(t, viewList, out.view, "Esc → viewList")
	assert.Equal(t, int64(7), out.scope.projectID, "scope unchanged")
	assert.Nil(t, cmd, "no refetch on Esc-back")
}

// TestProjectsView_EscNoOpOnBootEntry pins that Esc with no prior scope
// (boot landed on viewProjects) leaves the view in place. Spec §1.4.
func TestProjectsView_EscNoOpOnBootEntry(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView()
	// Default scope is zero (empty=false, projectID=0, allProjects=false)
	// — this represents the boot landing case.

	out, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.Equal(t, viewProjects, out.view, "Esc with no prior list → no transition")
	assert.Nil(t, cmd)
}

// TestProjectsView_RRefreshes pins spec §1.4: r dispatches a manual
// refresh of the projects table. View stays in viewProjects.
func TestProjectsView_RRefreshes(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupProjectsView()

	out, cmd := m.routeProjectsViewKey(keyRune('r'))
	assert.Equal(t, viewProjects, out.view)
	require.NotNil(t, cmd, "r must dispatch fetchProjectsWithStats")
}

// TestProjectsView_PFromListTransitions pins spec §1.4: P from viewList
// transitions to viewProjects and dispatches the stats fetch. Scope is
// preserved on the way out so an Esc-back returns to the same queue.
func TestProjectsView_PFromListTransitions(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := initialModel(Options{})
	m.view = viewList
	m.scope = homedScope(7, "kata")
	// Need a stub api so the cmd can be dispatched without crashing —
	// the cmd doesn't run to completion in this test.
	m.api = &Client{}

	nm, cmd := updateModel(m, keyRune('P'))
	assert.Equal(t, viewProjects, nm.view)
	assert.Equal(t, int64(7), nm.scope.projectID, "scope preserved on P transition")
	require.NotNil(t, cmd, "P must dispatch a stats fetch")
}

// TestProjectsView_PWhileInputFocusedRoutesToPrompt pins spec §1.4: P
// while a search bar / form is focused reaches the prompt instead of
// transitioning the view.
func TestProjectsView_PWhileInputFocusedRoutesToPrompt(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := initialModel(Options{})
	m.view = viewList
	m.scope = scope{projectID: 7, projectName: "kata"}
	m.input = newSearchBar(ListFilter{})

	nm, _ := updateModel(m, keyRune('P'))
	assert.Equal(t, viewList, nm.view, "view must not transition while input is focused")
	if v := nm.input.activeField().value(); v != "P" {
		t.Fatalf("input buffer = %q, want %q", v, "P")
	}
}

// inboxSplitWithOpenDetail drives a model through the real I-key path
// into the Inbox scope in split layout, then seeds a detail pane open
// on the Inbox project's issue (the pane a split user sees while
// browsing). Returns the model plus the seeded issue so tests can
// assert on exactly that stale state.
func inboxSplitWithOpenDetail(t *testing.T) (Model, Issue) {
	t.Helper()
	inbox := ProjectSummary{ID: 2, Name: "capture-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 7, Name: "example-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(7, "example-project")
	m.width, m.height = 120, 30
	m = resizeModel(m, 160, 40)
	injectProjects(&m,
		mockProject{ID: 2, Name: "capture-project", Ident: "..."},
		mockProject{ID: 5, Name: "spoke-project", Ident: "..."},
	)
	m = enterInboxForTest(t, m)

	// The split detail pane is open on the Inbox project's issue with a
	// generation from that open (unit fixture for the pane a user sees
	// before pressing P).
	oldIssue := Issue{ProjectID: 2, UID: "01TEST-old9", ShortID: "old9", Title: "Old inbox issue", Status: "open"}
	m.detail = detailModel{issue: &oldIssue, scopePID: 2, gen: 41, loading: true}
	m.nextGen = 41
	return m, oldIssue
}

// cursorForTestProject returns the projects-view row index for the
// given project id, failing the test if the row is missing.
func cursorForTestProject(t *testing.T, m Model, projectID int64) int {
	t.Helper()
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	for i, r := range rows {
		if !r.sentinel && r.projectID == projectID {
			return i
		}
	}
	t.Fatalf("project %d missing from projects rows", projectID)
	return 0
}

// Selecting another project or All projects clears the previous detail pane
// so detail actions cannot target a task from the old scope.
func TestProjectsView_SelectionClearsStaleSplitDetail(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	for _, tc := range []struct {
		name      string
		wantAll   bool
		wantPID   int64
		cursorFor func(t *testing.T, m Model) int
		wantScope string
	}{
		{
			name: "real project", wantAll: false, wantPID: 5,
			cursorFor: func(t *testing.T, m Model) int { return cursorForTestProject(t, m, 5) },
		},
		{
			name: "all projects sentinel", wantAll: true, wantPID: 0,
			cursorFor: func(*testing.T, Model) int { return 0 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := inboxSplitWithOpenDetail(t)
			require.NotNil(t, m.inboxReturn)
			followGenBefore := m.nextDetailFollowGen

			m, _ = updateModel(m, keyRune('P'))
			require.Equal(t, viewProjects, m.view)
			m.projectsCursor = tc.cursorFor(t, m)

			m, selectFetch := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			require.NotNil(t, selectFetch, "scope-changing selection must dispatch a fetch")
			require.Equal(t, viewList, m.view)
			require.Equal(t, tc.wantAll, m.scope.allProjects)
			require.Equal(t, tc.wantPID, m.scope.projectID)
			require.Nil(t, m.inboxReturn, "scope change discards the saved Inbox return state")
			require.True(t, m.cache.isStale(), "issue cache must be invalidated on scope change")

			require.Nil(t, m.detail.issue,
				"the prior scope's detail issue must not survive the scope change")
			require.Zero(t, m.detail.scopePID,
				"the prior scope's detail project pin must not survive the scope change")
			require.Equal(t, followGenBefore+1, m.nextDetailFollowGen,
				"a pending detail-follow tick from the old scope must be fenced")
		})
	}
}

// A detail reply from before a project selection must not repopulate
// the cleared pane.
func TestProjectsView_SelectionDropsStaleDetailResponse(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m, oldIssue := inboxSplitWithOpenDetail(t)

	m, _ = updateModel(m, keyRune('P'))
	m.projectsCursor = cursorForTestProject(t, m, 5)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, m.detail.issue)

	m, _ = updateModel(m, detailFetchedMsg{gen: 41, issue: &oldIssue})
	require.Nil(t, m.detail.issue,
		"a pre-selection detail response must not repopulate the pane after the scope change")
}

// TestProjectsView_SelectionFencesPendingFollowTick pins that a
// detail-follow debounce tick armed before a scope-changing selection
// is fenced by the selection: it must not dispatch detail fetches
// against the old scope's issue.
func TestProjectsView_SelectionFencesPendingFollowTick(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m, _ := inboxSplitWithOpenDetail(t)
	pendingGen := m.nextDetailFollowGen

	m, _ = updateModel(m, keyRune('P'))
	m.projectsCursor = cursorForTestProject(t, m, 5)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})

	_, tickCmd := updateModel(m, detailFollowTickMsg{gen: pendingGen})
	require.Nil(t, tickCmd,
		"a pre-selection follow tick must be fenced instead of fetching the old scope's issue")
}

// After a project selection, the fresh list opens the highlighted task
// in the split detail pane.
func TestProjectsView_SelectionBootstrapsDetailFromFreshList(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	for _, tc := range []struct {
		name      string
		wantAll   bool
		cursorFor func(t *testing.T, m Model) int
	}{
		{
			name: "real project", wantAll: false,
			cursorFor: func(t *testing.T, m Model) int { return cursorForTestProject(t, m, 5) },
		},
		{
			name: "all projects sentinel", wantAll: true,
			cursorFor: func(*testing.T, Model) int { return 0 },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &inboxTestAPI{}
			m, _ := inboxSplitWithOpenDetail(t)
			m.api = api

			m, _ = updateModel(m, keyRune('P'))
			m.projectsCursor = tc.cursorFor(t, m)
			m, selectFetch := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Nil(t, m.detail.issue)

			fresh := Issue{ProjectID: 5, UID: "01TEST-nnp1", ShortID: "nnp1", Title: "Fresh row", Status: "open"}
			api.issues = []Issue{fresh}
			selectMsg, ok := selectFetch().(initialFetchMsg)
			require.True(t, ok, "selection fetch must be an initialFetchMsg")
			m, _ = updateModel(m, selectMsg)

			require.NotNil(t, m.detail.issue,
				"the accepted fresh list response must bootstrap the detail pane")
			require.Equal(t, "nnp1", m.detail.issue.ShortID,
				"the pane follows the newly selected scope's highlighted row")
			require.Equal(t, int64(5), m.detail.scopePID,
				"the pane pins the new scope's project id")
			require.Equal(t, tc.wantAll, m.scope.allProjects)
		})
	}
}

// Reselecting a project after visiting Inbox must reject old list replies
// from before Inbox entry, even when they name the same project.
func TestProjectsViewSelectionDropsPreInboxFetchForSameProject(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	inbox := ProjectSummary{ID: 2, Name: "capture-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 7, Name: "example-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-stale", ProjectID: 7, ShortID: "stale", Title: "Stale queue task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = homedScope(7, "example-project")
	m.width, m.height = 120, 30
	injectProjects(&m,
		mockProject{ID: 2, Name: "capture-project", Ident: "..."},
		mockProject{ID: 7, Name: "example-project", Ident: "..."},
	)

	// A pre-Inbox list fetch for project 7 is in flight.
	oldFetch := m.fetchInitial()
	require.NotNil(t, oldFetch)
	oldResult := oldFetch()

	// Enter the Inbox, then open the projects view and select the same
	// ordinary project the user came from.
	api.issues = []Issue{
		{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
	}
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, inboxFetch := updateModel(m, lookup())
	require.NotNil(t, inboxFetch)
	m, _ = updateModel(m, inboxFetch())
	require.True(t, m.scope.inbox)

	m, _ = updateModel(m, keyRune('P'))
	require.Equal(t, viewProjects, m.view)

	// Rows: sentinel, then name-ascending (no events): capture-project
	// (ID 2), example-project (ID 7). Select the example-project row.
	m.projectsCursor = 2
	m, selectFetch := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, selectFetch, "scope-changing selection must dispatch a fetch")
	require.False(t, m.scope.inbox)
	require.Equal(t, int64(7), m.scope.projectID)

	// The fresh selection fetch lands first.
	api.issues = []Issue{
		{UID: "01TEST-fresh", ProjectID: 7, ShortID: "fresh", Title: "Fresh queue task", Status: "open"},
	}
	m, _ = updateModel(m, selectFetch())
	require.NotEmpty(t, m.list.issues)
	require.Equal(t, "Fresh queue task", m.list.issues[0].Title)

	// The stale pre-Inbox reply lands after the fresh selection result.
	m, _ = updateModel(m, oldResult)
	require.NotEmpty(t, m.list.issues)
	require.Equal(t, "Fresh queue task", m.list.issues[0].Title,
		"a pre-Inbox reply landing after the selection fetch must not overwrite the fresh list")
	require.NotEmpty(t, m.cache.data)
	require.Equal(t, "Fresh queue task", m.cache.data[0].Title,
		"a pre-Inbox reply landing after the selection fetch must not overwrite the fresh cache")
}

// Selecting All projects after visiting Inbox must reject all-projects
// replies from before Inbox entry.
func TestProjectsViewSelectionDropsPreInboxFetchForAllProjectsScope(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	inbox := ProjectSummary{ID: 2, Name: "capture-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	api := &inboxTestAPI{
		projects: []ProjectSummary{{ID: 7, Name: "example-project"}, inbox},
		issues: []Issue{
			{UID: "01TEST-stale", ProjectID: 7, ShortID: "stale", Title: "Stale queue task", Status: "open"},
		},
	}
	m := newTestModel()
	m.api = api
	m.scope = scope{allProjects: true}
	m.width, m.height = 120, 30
	injectProjects(&m,
		mockProject{ID: 2, Name: "capture-project", Ident: "..."},
		mockProject{ID: 7, Name: "example-project", Ident: "..."},
	)

	// A pre-Inbox all-projects list fetch is in flight.
	oldFetch := m.fetchInitial()
	require.NotNil(t, oldFetch)
	oldResult := oldFetch()

	// Enter the Inbox, then open the projects view and select the
	// All-projects sentinel.
	api.issues = []Issue{
		{UID: "01TEST-inbox", ProjectID: 2, ShortID: "inbox", Title: "Capture task", Status: "open"},
	}
	m, lookup := updateModel(m, keyRune('I'))
	require.NotNil(t, lookup)
	m, inboxFetch := updateModel(m, lookup())
	require.NotNil(t, inboxFetch)
	m, _ = updateModel(m, inboxFetch())
	require.True(t, m.scope.inbox)

	m, _ = updateModel(m, keyRune('P'))
	require.Equal(t, viewProjects, m.view)

	m.projectsCursor = 0 // sentinel row
	m, selectFetch := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, selectFetch, "scope-changing selection must dispatch a fetch")
	require.False(t, m.scope.inbox)
	require.True(t, m.scope.allProjects)

	// The fresh selection fetch lands first.
	api.issues = []Issue{
		{UID: "01TEST-fresh", ProjectID: 7, ShortID: "fresh", Title: "Fresh queue task", Status: "open"},
	}
	m, _ = updateModel(m, selectFetch())
	require.NotEmpty(t, m.list.issues)
	require.Equal(t, "Fresh queue task", m.list.issues[0].Title)

	// The stale pre-Inbox reply lands after the fresh selection result.
	m, _ = updateModel(m, oldResult)
	require.NotEmpty(t, m.list.issues)
	require.Equal(t, "Fresh queue task", m.list.issues[0].Title,
		"a pre-Inbox reply landing after the selection fetch must not overwrite the fresh list")
	require.NotEmpty(t, m.cache.data)
	require.Equal(t, "Fresh queue task", m.cache.data[0].Title,
		"a pre-Inbox reply landing after the selection fetch must not overwrite the fresh cache")
}

package tui

import (
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
func TestProjectsView_RendersWithoutPanic(t *testing.T) {
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
	for i := 0; i < 50; i++ {
		rows := projectsRows(byID, idents, stats)
		require.Len(t, rows, 3)
		assert.Equal(t, int64(50), rows[1].projectID, "lower projectID first on tie")
		assert.Equal(t, int64(100), rows[2].projectID)
	}
}

// TestProjectsView_RendersTable confirms the table renders with the
// expected column headers and row content for a fixture model. Wide
// terminal so all columns fit.
func TestProjectsView_RendersTable(t *testing.T) {
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

func TestProjectsView_FooterUsesAdaptiveHelpTable(t *testing.T) {
	m := setupProjectsView(mockProject{ID: 1, Name: "alpha", Ident: "..."})

	out := stripANSI(m.viewContent())

	assert.Contains(t, out, "▕")
	assert.Contains(t, out, "F federation")
	assert.NotContains(t, out, "[F] federation")
}

func TestProjectsView_FTransitionsToFederationWithHighlightedProjectSelected(t *testing.T) {
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
	assert.Equal(t, federationModeSelectLocalProject, out.federationMode)
	rows := federationLocalProjectRows(out)
	require.Greater(t, len(rows), out.federationLocalProjectCursor)
	require.False(t, rows[out.federationLocalProjectCursor].createReplica)
	assert.Equal(t, "beta-project", rows[out.federationLocalProjectCursor].project.Name)

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Equal(t, int64(22), out.federationDraft.SpokeProjectID)
	assert.Equal(t, "beta-project", out.federationDraft.SpokeProjectName)
}

func TestProjectsView_FTransitionSelectedProjectIgnoresStaleFederationDraft(t *testing.T) {
	m := setupProjectsView(
		mockProject{ID: 11, Name: "alpha-project", Ident: "..."},
		mockProject{ID: 22, Name: "beta-project", Ident: "..."},
	)
	m.projectsCursor = 2
	m.federationDraft.SpokeProjectName = "stale-project"

	out, cmd := updateModel(m, keyRune('F'))

	assert.Equal(t, viewFederation, out.view)
	require.NotNil(t, cmd)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "selected project: beta-project")
	assert.NotContains(t, rendered, "selected project: stale-project")
}

func TestProjectsView_FFromAllProjectsHasNoSelectedFederationProject(t *testing.T) {
	m := setupProjectsView(mockProject{ID: 11, Name: "alpha-project", Ident: "..."})
	m.scope = homedScope(99, "previous-project")
	m.projectsCursor = 0

	out, cmd := updateModel(m, keyRune('F'))

	assert.Equal(t, viewFederation, out.view)
	require.NotNil(t, cmd)
	assert.Contains(t, stripANSI(renderFederation(out)), "selected project: none")

	out, cmd = out.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectLocalProject, out.federationMode)
}

// TestProjectsView_ViewportClipsRowsToHeight pins that with many
// projects and a small terminal, the footer + key-hint line stay on
// screen. Without clipping, every row renders and the chrome falls
// off the bottom — the user can't see [↑/↓ k/j] move etc.
func TestProjectsView_ViewportClipsRowsToHeight(t *testing.T) {
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
func TestProjectsView_DashWhenNoEvents(t *testing.T) {
	m := setupProjectsView(
		mockProject{ID: 1, Name: "fresh", Ident: "github.com/wesm/fresh"},
	)
	out := m.viewContent()
	assert.Contains(t, out, "—", "nil LastEventAt must render as em-dash")
}

// TestProjectsView_ProjectFooterOnHighlight pins spec §5.1:
// highlighting a real project renders its name beneath the table;
// highlighting the sentinel renders the description.
func TestProjectsView_ProjectFooterOnHighlight(t *testing.T) {
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
func TestProjectsView_JKMoveCursor(t *testing.T) {
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
func TestProjectsView_EnterOnProjectTransitions(t *testing.T) {
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
func TestProjectsView_EnterOnSentinelTransitions(t *testing.T) {
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
func TestProjectsView_EnterOnCurrentScopeIsIdempotent(t *testing.T) {
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
func TestProjectsView_EscReturnsToPriorList(t *testing.T) {
	m := setupProjectsView()
	m.scope = homedScope(7, "kata")

	out, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.Equal(t, viewList, out.view, "Esc → viewList")
	assert.Equal(t, int64(7), out.scope.projectID, "scope unchanged")
	assert.Nil(t, cmd, "no refetch on Esc-back")
}

// TestProjectsView_EscNoOpOnBootEntry pins that Esc with no prior scope
// (boot landed on viewProjects) leaves the view in place. Spec §1.4.
func TestProjectsView_EscNoOpOnBootEntry(t *testing.T) {
	m := setupProjectsView()
	// Default scope is zero (empty=false, projectID=0, allProjects=false)
	// — this represents the boot landing case.

	out, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.Equal(t, viewProjects, out.view, "Esc with no prior list → no transition")
	assert.Nil(t, cmd)
}

// TestProjectsView_RRefreshes pins spec §1.4: r dispatches a manual
// refresh of the projects table. View stays in viewProjects.
func TestProjectsView_RRefreshes(t *testing.T) {
	m := setupProjectsView()

	out, cmd := m.routeProjectsViewKey(keyRune('r'))
	assert.Equal(t, viewProjects, out.view)
	require.NotNil(t, cmd, "r must dispatch fetchProjectsWithStats")
}

// TestProjectsView_PFromListTransitions pins spec §1.4: P from viewList
// transitions to viewProjects and dispatches the stats fetch. Scope is
// preserved on the way out so an Esc-back returns to the same queue.
func TestProjectsView_PFromListTransitions(t *testing.T) {
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
func TestProjectsView_PWhileInputFocusedRoutesToPrompt(t *testing.T) {
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

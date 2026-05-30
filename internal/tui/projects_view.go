package tui

import (
	"context"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fetchProjectsWithStats issues GET /api/v1/projects?include=stats and
// dispatches a projectsLoadedMsg carrying the stats map. The boot
// fetchProjects cmd is the no-stats variant used by the list view's
// project-name cache; this cmd is dispatched by every transition into
// viewProjects per spec §6.2.
//
// Failures populate err so the message handler can surface a toast
// without leaving the table empty.
//
// gen captures m.projectsGen at dispatch time and rides on the response
// so the projectsLoadedMsg handler can detect whether an SSE
// invalidation bumped the counter while the fetch was in flight — only
// a response whose gen still matches m.projectsGen is allowed to clear
// m.projectsStale. Spec §6.3.
func (m Model) fetchProjectsWithStats() tea.Cmd {
	api := m.api
	gen := m.projectsGen
	connGen := m.connGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := api.ListProjectsWithStats(ctx)
		if err != nil {
			return projectsLoadedMsg{connGen: connGen, err: err, gen: gen}
		}
		names := make(map[int64]string, len(rows))
		idents := make(map[int64]string, len(rows))
		stats := make(map[int64]ProjectStatsSummary, len(rows))
		for _, r := range rows {
			names[r.ID] = r.Name
			idents[r.ID] = r.Name
			if r.Stats != nil {
				stats[r.ID] = *r.Stats
			}
		}
		return projectsLoadedMsg{
			connGen:  connGen,
			projects: names,
			idents:   idents,
			stats:    stats,
			gen:      gen,
		}
	}
}

// projectsRow is one row of the projects view. Sentinel=true marks the
// implicit "All projects" entry at index 0; otherwise the row carries a
// concrete projectID.
type projectsRow struct {
	sentinel  bool
	projectID int64
	name      string
	stats     ProjectStatsSummary
}

// projectsRows builds the row list rendered by viewProjects. The
// sentinel row is always at index 0; remaining rows are sorted by
// last_event_at desc with name asc as the tiebreak. Spec §5.3.
//
// The sentinel's totals are client-summed from the per-row stats (spec
// §1.6) so the "All projects" Open/Closed/Total are guaranteed
// consistent with the rows on the same frame, and last_event_at is the
// max across rows.
func projectsRows(byID map[int64]string, _ map[int64]string, stats map[int64]ProjectStatsSummary) []projectsRow {
	rows := []projectsRow{}
	for id, name := range byID {
		rows = append(rows, projectsRow{
			projectID: id,
			name:      name,
			stats:     stats[id],
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := timeOrZero(rows[i].stats.LastEventAt), timeOrZero(rows[j].stats.LastEventAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		ni, nj := strings.ToLower(rows[i].name), strings.ToLower(rows[j].name)
		if ni != nj {
			return ni < nj
		}
		return rows[i].projectID < rows[j].projectID
	})
	sentinel := projectsRow{sentinel: true, name: "All projects"}
	var maxT time.Time
	for _, r := range rows {
		sentinel.stats.Open += r.stats.Open
		sentinel.stats.Closed += r.stats.Closed
		if r.stats.LastEventAt != nil && r.stats.LastEventAt.After(maxT) {
			maxT = *r.stats.LastEventAt
		}
	}
	if !maxT.IsZero() {
		sentinel.stats.LastEventAt = &maxT
	}
	return append([]projectsRow{sentinel}, rows...)
}

// timeOrZero unwraps an optional time pointer, returning the zero value
// for nil. Sort uses this so a project with no events sinks to the end
// of the descending order.
func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// routeProjectsViewKey delivers a key to the active projects view.
// j/k or up/down move the cursor (clamped); g/G or Home/End jump;
// Enter selects the highlighted row and transitions to viewList; Esc
// returns to the prior list view (or no-op if there isn't one); r
// dispatches a manual refresh. Other keys are absorbed.
//
// Spec §1.4 (P/Esc/r), §5.4 (Enter), §6.2 (transition-driven refetch).
func (m Model) routeProjectsViewKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	if next, ok := m.cursorMoveProjects(msg, rows); ok {
		return next, nil
	}
	switch msg.String() {
	case "esc":
		return m.escFromProjectsView()
	case "r":
		return m, m.fetchProjectsWithStats()
	case "enter":
		return m.applyProjectsViewSelection(rows)
	}
	return m, nil
}

// cursorMoveProjects handles the four cursor-movement keys for the
// projects view. Returns (model, true) if the key matched a movement;
// (model, false) otherwise so the caller can dispatch the remaining
// keys. Extracted from routeProjectsViewKey to keep both functions
// under the project's cyclomatic complexity ceiling (≤8).
func (m Model) cursorMoveProjects(msg tea.KeyMsg, rows []projectsRow) (Model, bool) {
	switch msg.String() {
	case "j", "down":
		if m.projectsCursor < len(rows)-1 {
			m.projectsCursor++
		}
		return m, true
	case "k", "up":
		if m.projectsCursor > 0 {
			m.projectsCursor--
		}
		return m, true
	case "g", "home":
		m.projectsCursor = 0
		return m, true
	case "G", "end":
		m.projectsCursor = len(rows) - 1
		return m, true
	}
	return m, false
}

// applyProjectsViewSelection commits the highlighted row's choice. The
// sentinel transitions to all-projects scope; a real row transitions to
// single-project scope. Either way the issue cache is dropped and a
// fresh issue fetch is dispatched. Spec §5.4.
//
// Idempotent re-selection: if the highlighted row already matches the
// active scope, just return to viewList without invalidating the cache
// or refetching, so the user can re-confirm the current scope without
// churn.
func (m Model) applyProjectsViewSelection(rows []projectsRow) (Model, tea.Cmd) {
	if m.projectsCursor < 0 || m.projectsCursor >= len(rows) {
		return m, nil
	}
	r := rows[m.projectsCursor]
	if projectsSelectionMatchesScope(r, m.scope) {
		m.view = viewList
		return m, nil
	}
	if r.sentinel {
		m.scope = scope{allProjects: true}
	} else {
		m.scope = scope{
			projectID:       r.projectID,
			projectName:     r.name,
			homeProjectID:   r.projectID,
			homeProjectName: r.name,
		}
	}
	m.view = viewList
	m.list = listModel{actor: m.list.actor}
	m.cache.markStale()
	return m, m.fetchInitial()
}

// projectsSelectionMatchesScope reports whether selecting r would
// produce the same scope already in s. Used by
// applyProjectsViewSelection to skip cache invalidation when the user
// re-selects the current scope.
func projectsSelectionMatchesScope(r projectsRow, s scope) bool {
	if r.sentinel {
		return s.allProjects
	}
	return !s.allProjects && r.projectID == s.projectID
}

// escFromProjectsView returns to the prior viewList if scope is set
// (the user came from a list); otherwise it's a no-op (boot landed
// here without a prior list view). Spec §1.4. The cached list is
// reused — no refetch.
func (m Model) escFromProjectsView() (Model, tea.Cmd) {
	if m.scope.projectID == 0 && !m.scope.allProjects {
		return m, nil // boot landing, no prior list
	}
	m.view = viewList
	return m, nil
}

// transitionToProjects switches to viewProjects and dispatches a stats
// fetch per spec §6.2. Cursor positions on the row matching the active
// scope so a no-op P → Esc round-trip leaves the cursor where the user
// expects. When scope.allProjects is true, cursor lands on the sentinel.
func (m Model) transitionToProjects() (Model, tea.Cmd) {
	m.view = viewProjects
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	m.projectsCursor = cursorForScope(rows, m.scope)
	return m, m.fetchProjectsWithStats()
}

// cursorForScope finds the row matching the active scope. Returns 0
// (sentinel) when scope.allProjects, the row matching scope.projectID
// when set, or 0 as a safe default.
func cursorForScope(rows []projectsRow, sc scope) int {
	if sc.allProjects {
		return 0
	}
	for i, r := range rows {
		if !r.sentinel && r.projectID == sc.projectID {
			return i
		}
	}
	return 0
}

const projectsRefetchDebounce = 500 * time.Millisecond

// projectsDebounceFireMsg is the wakeup the debounce timer dispatches
// after projectsRefetchDebounce elapses since the last stale-flip.
type projectsDebounceFireMsg struct{}

// projectsDebounceCmd schedules the debounce wakeup. The Update handler
// for projectsDebounceFireMsg consumes the message and either dispatches
// fetchProjectsWithStats (when viewProjects is still active) or no-ops.
// Spec §6.3.
func projectsDebounceCmd() tea.Cmd {
	return tea.Tick(projectsRefetchDebounce, func(time.Time) tea.Msg {
		return projectsDebounceFireMsg{}
	})
}

// eventAffectsProjectsTable reports whether an incoming SSE event
// should mark a viewProjects table stale. Any project-scoped event
// (project_id != 0) qualifies: known projects' Open/Closed/Updated
// columns may have changed, and unknown-project events are exactly
// the signal that a new project has appeared and the table needs to
// learn about it on the next fetchProjectsWithStats. Global events
// (project_id == 0) cannot affect per-project stats.
func eventAffectsProjectsTable(msg eventReceivedMsg) bool {
	return msg.projectID != 0
}

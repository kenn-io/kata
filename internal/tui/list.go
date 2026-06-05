package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// listAPI is the subset of *Client the list-view Update path needs.
// Defining it as an interface (instead of taking *Client directly) lets
// list_filter_test.go drive Update with a fake without standing up a
// httptest server — the client tests already cover the wire format.
//
// ref is a short_id, qualified short_id, or 26-char ULID — the daemon's
// path resolver accepts all three.
type listAPI interface {
	ListIssues(ctx context.Context, projectID int64, f ListFilter) ([]Issue, error)
	ListAllIssues(ctx context.Context, f ListFilter) ([]Issue, error)
	CreateIssue(
		ctx context.Context, projectID int64, body CreateIssueBody,
	) (*MutationResp, error)
	Close(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error)
	Reopen(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error)
	SetPriority(
		ctx context.Context, projectID int64, ref string, priority *int64, actor string,
	) (*MutationResp, error)
}

// listModel owns list-view state: the current rows, cursor, the
// filter in effect, the resolved actor for mutations, and a one-shot
// status line that the View renders inside the chrome's info-line
// slot. The keymap lives on the parent Model and is passed into
// Update; one instance keeps the help view in lockstep with what
// handlers actually do.
//
// selectedUID tracks the issue.UID under the cursor for identity-
// based selection: when a refetch reorders rows (issues are sorted
// by updated_at DESC, so any background mutation can shuffle them),
// the cursor is restored onto the same issue rather than the same
// index. UID is canonical and survives short_id cutovers. Empty
// means "no selection" (empty list, pre-fetch state).
//
// M3.5c retired lm.search and lm.pendingTitle: the inline command
// bar (M3a) covers search/owner; the inline new-issue row (M3.5c)
// covers `n`. All input flows now live on Model.input.
type listModel struct {
	issues            []Issue
	cursor            int
	selectedUID       string
	selectedProjectID int64
	filter            ListFilter
	expanded          expansionSet
	actor             string
	status            string
	err               error
	loading           bool
	truncated         bool
	viewMode          issueListViewMode
	childSort         childSortMode
	pageRows          int
	windowStart       int
	// pendingPriority arms the next keystroke to set/clear the priority
	// of the highlighted row. Set when the user presses `!`; consumed
	// when the next key is 0..4 (set) or `-` (clear); reset by any other
	// key. The status line carries the hint while armed so the user can
	// see what kata is waiting for. Mirrors vim's modifier-then-motion
	// pattern but for a small finite priority space.
	pendingPriority bool
}

const queueWorkingSetLimit = 2000
const queueFetchLimit = queueWorkingSetLimit + 1

func queueFetchFilter() ListFilter {
	return ListFilter{Limit: queueFetchLimit}
}

func trimQueueWorkingSet(issues []Issue) ([]Issue, bool) {
	if len(issues) <= queueWorkingSetLimit {
		return issues, false
	}
	return issues[:queueWorkingSetLimit], true
}

// newListModel returns a listModel waiting for its first fetch. loading=true
// keeps the spinner-equivalent on screen until initialFetchMsg lands.
func newListModel() listModel {
	return listModel{loading: true}
}

// Update handles list-view keys and fetch results. The top-level
// Model keeps responsibility for global keys (q, ?, R), input shells
// (Model.input), modals (Model.modal), and SSE messages.
//
// initialFetchMsg/refetchedMsg are also applied by Model.populateCache
// before dispatch (so help/detail-overlay refetches keep lm.issues in
// sync); applyFetched is idempotent so re-applying here for the
// viewList path — and from drainCmd-style tests that drive lm.Update
// directly — is harmless.
func (lm listModel) Update(
	msg tea.Msg, km keymap, api listAPI, sc scope,
) (listModel, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyMsg:
		return lm.applyNavKey(m, km, api, sc)
	case initialFetchMsg, refetchedMsg:
		lm = lm.applyFetched(m)
	case mutationDoneMsg:
		return lm.applyMutation(m, api, sc)
	}
	return lm, nil
}

// applyNavKey routes a top-level keystroke through the cursor, filter,
// and prompt handlers. It returns early when a sub-handler reports it
// has consumed the key so the cyclomatic budget per function stays
// inside the project's ≤8 limit.
func (lm listModel) applyNavKey(
	msg tea.KeyMsg, km keymap, api listAPI, sc scope,
) (listModel, tea.Cmd) {
	// pendingPriority is checked first so the next keystroke after `!`
	// is consumed by the priority handler regardless of what else it
	// might bind to (e.g. `0` won't trigger Home, `-` won't trigger
	// RemoveLabel).
	if lm.pendingPriority {
		next, cmd, ok := lm.applyPendingPriorityKey(msg, api, sc)
		if ok {
			return next, cmd
		}
	}
	if next, ok := lm.applyCursorKey(msg, km); ok {
		return next, nil
	}
	if next, ok := lm.applyViewModeKey(msg, km); ok {
		return next, nil
	}
	if next, ok := lm.applyExpandAllKey(msg, km); ok {
		return next, nil
	}
	if next, ok := lm.applyExpandKey(msg, km); ok {
		return next, nil
	}
	if next, ok := lm.applyChildSortKey(msg, km); ok {
		return next, nil
	}
	if next, cmd, ok := lm.applyFilterKey(msg, km); ok {
		return next, cmd
	}
	if next, cmd, ok := lm.applyPromptKey(msg, km, sc); ok {
		return next, cmd
	}
	if next, cmd, ok := lm.applyMutationKey(msg, km, api, sc); ok {
		return next, cmd
	}
	if next, cmd, ok := lm.applyOpenKey(msg, km); ok {
		return next, cmd
	}
	return lm, nil
}

// applyMutationKey handles list-side mutation bindings: close (x),
// reopen (r), and set-priority (!) act on the highlighted row. Empty
// list is a quiet no-op so a stray keystroke on the empty-state hint
// does nothing. `!` arms pendingPriority; the next 0..4 / `-` keystroke
// is consumed by applyPendingPriorityKey before it reaches any other
// handler.
func (lm listModel) applyMutationKey(
	msg tea.KeyMsg, km keymap, api listAPI, sc scope,
) (listModel, tea.Cmd, bool) {
	switch {
	case km.Close.matches(msg):
		next, cmd := lm.dispatchListClose(api, sc)
		return next, cmd, true
	case km.Reopen.matches(msg):
		next, cmd := lm.dispatchListReopen(api, sc)
		return next, cmd, true
	case km.SetPriority.matches(msg):
		next := lm.armPendingPriority()
		return next, nil, true
	}
	return lm, nil, false
}

// armPendingPriority flips the listModel into priority-pending mode and
// drops a status hint so the user can see what kata is waiting for.
// Empty list / no target row is a quiet no-op so `!` on the empty-state
// banner does nothing.
func (lm listModel) armPendingPriority() listModel {
	iss, ok := lm.targetRow()
	if !ok {
		return lm
	}
	lm.pendingPriority = true
	lm.status = fmt.Sprintf("set priority of #%s (0-4 to set, - to clear, esc to cancel)", iss.ShortID)
	return lm
}

// applyPendingPriorityKey consumes the keystroke following `!`. Digits
// 0..4 dispatch a set; `-` clears; esc / any other key cancels (the
// pending mode flips off and the status hint clears). ok=true means the
// key was handled — the caller skips the rest of the dispatch chain.
func (lm listModel) applyPendingPriorityKey(
	msg tea.KeyMsg, api listAPI, sc scope,
) (listModel, tea.Cmd, bool) {
	lm.pendingPriority = false
	s := msg.String()
	switch s {
	case "0", "1", "2", "3", "4":
		n := int64(s[0] - '0')
		next, cmd := lm.dispatchListSetPriority(api, sc, &n)
		return next, cmd, true
	case "-":
		next, cmd := lm.dispatchListSetPriority(api, sc, nil)
		return next, cmd, true
	case "esc":
		lm.status = ""
		return lm, nil, true
	}
	// Any other key cancels the pending-priority mode but is NOT
	// otherwise consumed — let it flow through to its normal handler so
	// e.g. `j` after `!` still moves the cursor instead of being eaten.
	lm.status = ""
	return lm, nil, false
}

// dispatchListSetPriority fires SetPriority for the row under the
// cursor. Empty list is a no-op (returns lm unchanged with a nil cmd).
// nil priority clears; pointer to 0..4 sets.
func (lm listModel) dispatchListSetPriority(
	api listAPI, sc scope, priority *int64,
) (listModel, tea.Cmd) {
	iss, ok := lm.targetRow()
	if !ok {
		return lm, nil
	}
	lm.status = ""
	return lm, setPriorityListCmd(api, projectIDForRow(iss, sc), iss.ShortID, priority, lm.actor)
}

// dispatchListClose closes the issue under the cursor. Empty list is a
// no-op (returns lm unchanged with a nil cmd).
func (lm listModel) dispatchListClose(
	api listAPI, sc scope,
) (listModel, tea.Cmd) {
	iss, ok := lm.targetRow()
	if !ok {
		return lm, nil
	}
	lm.status = ""
	return lm, closeIssueCmd(api, projectIDForRow(iss, sc), iss.ShortID, lm.actor)
}

// dispatchListReopen mirrors dispatchListClose for the reopen action.
func (lm listModel) dispatchListReopen(
	api listAPI, sc scope,
) (listModel, tea.Cmd) {
	iss, ok := lm.targetRow()
	if !ok {
		return lm, nil
	}
	lm.status = ""
	return lm, reopenIssueCmd(api, projectIDForRow(iss, sc), iss.ShortID, lm.actor)
}

// targetRow returns the currently highlighted issue, accounting for the
// client-side filter that hides rows the cursor still indexes. ok=false
// when the visible list is empty.
func (lm listModel) targetRow() (Issue, bool) {
	row, ok := lm.targetQueueRow()
	if !ok {
		return Issue{}, false
	}
	return row.issue, true
}

func (lm listModel) targetQueueRow() (queueRow, bool) {
	rows := lm.visibleRows()
	if len(rows) == 0 {
		return queueRow{}, false
	}
	idx := lm.cursor
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	return rows[idx], true
}

func (lm listModel) visibleRows() []queueRow {
	return buildQueueRowsWithView(lm.issues, lm.filter, lm.expanded, lm.childSort, lm.viewMode)
}

// projectIDForRow picks the right project_id for the row's mutation.
// In all-projects scope the issue carries its own ProjectID; in single-
// project scope sc.projectID wins.
func projectIDForRow(iss Issue, sc scope) int64 {
	if sc.allProjects && iss.ProjectID != 0 {
		return iss.ProjectID
	}
	return sc.projectID
}

// closeIssueCmd wraps Close into a mutationDoneMsg-emitting tea.Cmd.
// origin="list" routes the response to listModel.applyMutation even if
// the user has switched to detail view between dispatch and arrival.
func closeIssueCmd(api listAPI, pid int64, ref, actor string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.Close(ctx, pid, ref, actor)
		return mutationDoneMsg{origin: "list", kind: "close", resp: resp, err: err}
	}
}

// reopenIssueCmd is the reopen counterpart of closeIssueCmd.
func reopenIssueCmd(api listAPI, pid int64, ref, actor string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.Reopen(ctx, pid, ref, actor)
		return mutationDoneMsg{origin: "list", kind: "reopen", resp: resp, err: err}
	}
}

// setPriorityListCmd wraps SetPriority into a mutationDoneMsg-emitting
// tea.Cmd. nil priority clears; non-nil sets to *priority. The
// mutation-kind discriminator ("priority.set" vs "priority.clear")
// drives the success-status template the same way the detail-side
// dispatch does.
func setPriorityListCmd(api listAPI, pid int64, ref string, priority *int64, actor string) tea.Cmd {
	kind := "priority.set"
	if priority == nil {
		kind = "priority.clear"
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.SetPriority(ctx, pid, ref, priority, actor)
		return mutationDoneMsg{origin: "list", kind: kind, resp: resp, err: err}
	}
}

// applyOpenKey handles Enter on a list row: emit openDetailMsg with the
// issue under the cursor. The top-level Model handles the message by
// switching to the detail view and dispatching the three tab fetches.
// Empty list (cursor would point past the slice) is a quiet no-op so a
// stray Enter on the empty-state hint does nothing.
func (lm listModel) applyOpenKey(
	msg tea.KeyMsg, km keymap,
) (listModel, tea.Cmd, bool) {
	if !km.Open.matches(msg) {
		return lm, nil, false
	}
	rows := lm.visibleRows()
	if len(rows) == 0 {
		return lm, nil, true
	}
	idx := lm.cursor
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	iss := rows[idx].issue
	return lm, func() tea.Msg { return openDetailMsg{issue: iss} }, true
}

// applyCursorKey handles the j/k/g/G/pgup/pgdown family. ok=true means
// the key was consumed. The cursor moves in filtered-space (the slice
// the user actually sees) so a hidden row preceding the cursor cannot
// desync the marker from the highlighted row. lm.cursor is therefore
// an index into lm.visibleRows().
//
// Each cursor change also updates lm.selectedUID so an SSE-driven
// refetch can put the cursor back on the same issue rather than the
// same index — see syncSelection.
func (lm listModel) applyCursorKey(msg tea.KeyMsg, km keymap) (listModel, bool) {
	rows := lm.visibleRows()
	n := len(rows)
	switch {
	case km.Up.matches(msg), km.ScrollUp.matches(msg):
		if lm.cursor > 0 {
			lm.cursor--
		}
	case km.Down.matches(msg), km.ScrollDown.matches(msg):
		if lm.cursor < n-1 {
			lm.cursor++
		}
	case km.PageUp.matches(msg):
		lm = lm.pageCursor(-1, n)
	case km.PageDown.matches(msg):
		lm = lm.pageCursor(1, n)
	case km.Home.matches(msg):
		lm.cursor = 0
		lm.windowStart = 0
	case km.End.matches(msg):
		if n > 0 {
			lm.cursor = n - 1
		}
	default:
		return lm, false
	}
	lm = lm.ensureCursorVisible(n)
	lm = lm.syncSelection(rows)
	return lm, true
}

func (lm listModel) applyExpandKey(msg tea.KeyMsg, km keymap) (listModel, bool) {
	if lm.viewMode == issueListViewFlat {
		return lm, false
	}
	switch {
	case km.ExpandCollapse.matches(msg):
		return lm.toggleExpanded(), true
	case km.Expand.matches(msg):
		return lm.setExpanded(true), true
	case km.Collapse.matches(msg):
		return lm.setExpanded(false), true
	}
	return lm, false
}

func (lm listModel) applyExpandAllKey(msg tea.KeyMsg, km keymap) (listModel, bool) {
	if !km.ExpandAll.matches(msg) {
		return lm, false
	}
	if lm.viewMode == issueListViewFlat {
		return lm, true
	}
	lm = lm.syncSelection(lm.visibleRows())
	keys := lm.expandableKeys()
	if lm.allExpandableKeysExpanded(keys) {
		lm.expanded = expansionSet{}
		lm.status = "collapsed all"
		return lm.restoreCursorToSelection(), true
	}
	lm.expanded = make(expansionSet, len(keys))
	for _, key := range keys {
		lm.expanded[key] = true
	}
	lm.status = "expanded all"
	return lm.restoreCursorToSelection(), true
}

func (lm listModel) allExpandableKeysExpanded(keys []issueKey) bool {
	if len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if !lm.expanded[key] {
			return false
		}
	}
	return true
}

func (lm listModel) toggleExpanded() listModel {
	row, ok := lm.targetQueueRow()
	if !ok || !row.hasChildren {
		return lm
	}
	return lm.setExpansion(row, !lm.expanded[row.key])
}

// setExpanded forces the targeted row to expanded (want=true) or
// collapsed (want=false). Leaves rows without children untouched and
// is a no-op when the row is already in the requested state, so the
// right/left arrow bindings stay idempotent.
func (lm listModel) setExpanded(want bool) listModel {
	row, ok := lm.targetQueueRow()
	if !ok || !row.hasChildren || lm.expanded[row.key] == want {
		return lm
	}
	return lm.setExpansion(row, want)
}

func (lm listModel) setExpansion(row queueRow, want bool) listModel {
	if lm.expanded == nil {
		lm.expanded = expansionSet{}
	}
	if want {
		lm.expanded[row.key] = true
	} else {
		delete(lm.expanded, row.key)
	}
	return lm
}

func (lm listModel) applyViewModeKey(msg tea.KeyMsg, km keymap) (listModel, bool) {
	if !km.ToggleIssueView.matches(msg) {
		return lm, false
	}
	lm = lm.syncSelection(lm.visibleRows())
	if lm.viewMode == issueListViewFlat {
		lm.viewMode = issueListViewNested
		lm.expanded = expansionSet{}
	} else {
		lm.viewMode = issueListViewFlat
	}
	lm.status = "view: " + lm.viewMode.label()
	return lm.restoreCursorToSelection(), true
}

func (lm listModel) applyChildSortKey(msg tea.KeyMsg, km keymap) (listModel, bool) {
	if !km.SortChildren.matches(msg) {
		return lm, false
	}
	if lm.viewMode == issueListViewFlat {
		return lm, true
	}
	lm = lm.syncSelection(lm.visibleRows())
	if lm.childSort == childSortTemporal {
		lm.childSort = childSortTopological
	} else {
		lm.childSort = childSortTemporal
	}
	lm.status = "graph order: " + lm.childSort.label()
	return lm.restoreCursorToSelection(), true
}

func (lm listModel) restoreCursorToSelection() listModel {
	rows := lm.visibleRows()
	if lm.selectedUID == "" {
		return lm.clampCursorToVisibleRows()
	}
	for i, row := range rows {
		if row.issue.UID == lm.selectedUID &&
			(lm.selectedProjectID == 0 || row.issue.ProjectID == lm.selectedProjectID) {
			return lm.selectVisibleRow(rows, i)
		}
	}
	if i, ok := lm.nearestVisibleAncestorIndex(rows); ok {
		return lm.selectVisibleRow(rows, i)
	}
	return lm.clampCursorToVisibleRows()
}

func (lm listModel) selectVisibleRow(rows []queueRow, idx int) listModel {
	lm.cursor = idx
	if idx >= 0 && idx < len(rows) {
		lm.selectedUID = rows[idx].issue.UID
		lm.selectedProjectID = rows[idx].issue.ProjectID
	}
	return lm.ensureCursorVisible(len(rows))
}

func (lm listModel) nearestVisibleAncestorIndex(rows []queueRow) (int, bool) {
	selectedKey, ok := lm.selectedIssueKey()
	if !ok {
		return 0, false
	}
	visibleByKey := make(map[issueKey]int, len(rows))
	for i, row := range rows {
		visibleByKey[row.key] = i
	}
	issuesByKey := make(map[issueKey]Issue, len(lm.issues))
	for _, iss := range lm.issues {
		issuesByKey[issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}] = iss
	}
	seen := map[issueKey]bool{selectedKey: true}
	for key := selectedKey; ; {
		iss, ok := issuesByKey[key]
		if !ok || iss.ParentShortID == nil {
			return 0, false
		}
		parentKey := issueKey{projectID: iss.ProjectID, shortID: *iss.ParentShortID}
		if idx, ok := visibleByKey[parentKey]; ok {
			return idx, true
		}
		if seen[parentKey] {
			return 0, false
		}
		seen[parentKey] = true
		key = parentKey
	}
}

func (lm listModel) selectedIssueKey() (issueKey, bool) {
	for _, iss := range lm.issues {
		if iss.UID == lm.selectedUID &&
			(lm.selectedProjectID == 0 || iss.ProjectID == lm.selectedProjectID) {
			return issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}, true
		}
	}
	return issueKey{}, false
}

func (lm listModel) expandableKeys() []issueKey {
	present := make(map[issueKey]bool, len(lm.issues))
	for _, iss := range lm.issues {
		present[issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}] = true
	}
	seen := map[issueKey]bool{}
	keys := []issueKey{}
	for _, iss := range lm.issues {
		if iss.ParentShortID == nil {
			continue
		}
		parentKey := issueKey{projectID: iss.ProjectID, shortID: *iss.ParentShortID}
		if !present[parentKey] || seen[parentKey] {
			continue
		}
		seen[parentKey] = true
		keys = append(keys, parentKey)
	}
	return keys
}

func (lm listModel) clampCursorToVisibleRows() listModel {
	rows := lm.visibleRows()
	if len(rows) == 0 {
		lm.cursor = 0
		lm.windowStart = 0
		lm.selectedUID = ""
		lm.selectedProjectID = 0
		return lm
	}
	if lm.cursor >= len(rows) {
		lm.cursor = len(rows) - 1
	}
	if lm.cursor < 0 {
		lm.cursor = 0
	}
	lm = lm.ensureCursorVisible(len(rows))
	lm.selectedUID = rows[lm.cursor].issue.UID
	lm.selectedProjectID = rows[lm.cursor].issue.ProjectID
	return lm
}

const listFallbackPageStep = 10
const listPageOverlap = 2

func listPageStepForRows(visibleRows int) int {
	if visibleRows <= 0 {
		return listFallbackPageStep
	}
	step := visibleRows - listPageOverlap
	if step < 1 {
		return 1
	}
	return step
}

func (lm listModel) pageCursor(direction, totalRows int) listModel {
	if totalRows == 0 {
		lm.cursor = 0
		lm.windowStart = 0
		return lm
	}
	if lm.pageRows <= 0 {
		lm.cursor += direction * listFallbackPageStep
		return lm.ensureCursorVisible(totalRows)
	}
	start, _ := lm.windowBounds(totalRows, lm.pageRows)
	screenRow := lm.cursor - start
	maxStart := max(0, totalRows-lm.pageRows)
	if direction > 0 && start == maxStart {
		lm.windowStart = maxStart
		lm.cursor = totalRows - 1
		return lm
	}
	if direction < 0 && start == 0 {
		lm.windowStart = 0
		lm.cursor = 0
		return lm
	}
	nextStart := start + direction*listPageStepForRows(lm.pageRows)
	if nextStart < 0 {
		nextStart = 0
	}
	if nextStart > maxStart {
		nextStart = maxStart
	}
	lm.windowStart = nextStart
	lm.cursor = nextStart + screenRow
	if lm.cursor >= totalRows {
		lm.cursor = totalRows - 1
	}
	if lm.cursor < 0 {
		lm.cursor = 0
	}
	return lm
}

func (lm listModel) ensureCursorVisible(totalRows int) listModel {
	if totalRows == 0 {
		lm.cursor = 0
		lm.windowStart = 0
		return lm
	}
	if lm.cursor < 0 {
		lm.cursor = 0
	}
	if lm.cursor >= totalRows {
		lm.cursor = totalRows - 1
	}
	if lm.pageRows <= 0 || totalRows <= lm.pageRows {
		lm.windowStart = 0
		return lm
	}
	if lm.windowStart < 0 {
		lm.windowStart = 0
	}
	maxStart := totalRows - lm.pageRows
	if lm.windowStart > maxStart {
		lm.windowStart = maxStart
	}
	if lm.cursor < lm.windowStart {
		lm.windowStart = lm.cursor
	}
	if lm.cursor >= lm.windowStart+lm.pageRows {
		lm.windowStart = lm.cursor - lm.pageRows + 1
	}
	return lm
}

func (lm listModel) windowBounds(totalRows, budget int) (int, int) {
	return windowBoundsFromStart(totalRows, lm.cursor, budget, lm.windowStart)
}

// syncSelection records the issue.UID under the cursor so a later
// refetch can restore the cursor onto the same issue rather than the
// same index. Empty filtered list clears selectedUID so we don't
// pin to a row that no longer exists.
func (lm listModel) syncSelection(rows []queueRow) listModel {
	if len(rows) == 0 {
		lm.selectedUID = ""
		lm.selectedProjectID = 0
		return lm
	}
	idx := lm.cursor
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	if idx < 0 {
		idx = 0
	}
	lm.selectedUID = rows[idx].issue.UID
	lm.selectedProjectID = rows[idx].issue.ProjectID
	return lm
}

// applyFilterKey handles s (cycle status) and c (clear). Filters apply
// over the cached all-status working set, so these keys do not refetch.
// The cursor is reset to 0 because the filtered-row count (and thus the
// index space lm.cursor lives in) changes with every filter adjustment.
//
// selectedUID is also cleared on each commit so the identity-
// based restore in applyFetched (after the refetch lands) doesn't
// fight the cursor=0 reset by jumping the cursor back to the
// previously-selected issue if it survived the new filter. The
// explicit "I changed the filter" intent overrides the implicit
// "follow the same issue across refetches" intent.
func (lm listModel) applyFilterKey(msg tea.KeyMsg, km keymap) (listModel, tea.Cmd, bool) {
	switch {
	case km.FilterStatus.matches(msg):
		lm.filter.Status = nextStatus(lm.filter.Status)
		lm.cursor = 0
		lm.windowStart = 0
		lm.selectedUID = ""
		lm.status = ""
		return lm, nil, true
	case km.ClearFilters.matches(msg):
		lm.filter = ListFilter{}
		lm.cursor = 0
		lm.windowStart = 0
		lm.selectedUID = ""
		lm.status = ""
		return lm, nil, true
	}
	return lm, nil, false
}

// applyPromptKey opens an input shell. `/` opens the inline search
// command bar (M3a); `f` opens the centered filter modal (Plan 8
// commit 5a — Status/Owner/Search axes); `n` opens the centered
// multi-field new-issue form (Plan 8 commit 4 — replaces the M3.5c
// inline row). All three hand off via openInputMsg so Model.openInput
// constructs the inputState centrally.
//
// The new-issue form is gated to non-all-projects scopes because the
// daemon's create endpoint is project-scoped; in all-projects mode
// (which is itself gated until daemon support lands) we surface a
// status hint instead of opening the form. The filter modal is NOT
// gated — filter-only modals work in cross-project mode too.
func (lm listModel) applyPromptKey(
	msg tea.KeyMsg, km keymap, sc scope,
) (listModel, tea.Cmd, bool) {
	switch {
	case km.Search.matches(msg):
		return lm, openInputCmd(inputSearchBar), true
	case km.FilterForm.matches(msg):
		return lm, openInputCmd(inputFilterForm), true
	case km.NewIssue.matches(msg):
		if sc.allProjects {
			lm.status = "cannot create from all-projects view; cd into a project"
			return lm, nil, true
		}
		return lm, openInputCmd(inputNewIssueForm), true
	case km.NewChild.matches(msg):
		if sc.allProjects {
			lm.status = "cannot create from all-projects view; cd into a project"
			return lm, nil, true
		}
		iss, ok := lm.targetRow()
		if !ok {
			return lm, nil, true
		}
		return lm, openNewChildInputCmd(iss.ShortID), true
	}
	return lm, nil, false
}

// openInputCmd emits openInputMsg{kind} so Model.routeTopLevel can
// hoist the state into m.input via the centralised openInput
// constructor. Sub-views never construct inputState directly — the
// centralisation keeps focus, snapshot/restore, and render
// integration in one place.
func openInputCmd(k inputKind) tea.Cmd {
	return func() tea.Msg { return openInputMsg{kind: k} }
}

func openNewChildInputCmd(parentShortID string) tea.Cmd {
	return func() tea.Msg {
		ref := parentShortID
		return openInputMsg{kind: inputNewIssueForm, parentShortID: &ref}
	}
}

// clampCursorToFilter recomputes lm.cursor against the visible
// visible row slice so a live filter change can't leave the cursor past
// the end. Used by Model.applyLiveBarFilter / commitInput / cancelInput
// when the bar mutates lm.filter on the user's behalf.
func (lm listModel) clampCursorToFilter() listModel {
	visible := len(lm.visibleRows())
	if visible == 0 {
		lm.cursor = 0
		lm.windowStart = 0
		return lm
	}
	if lm.cursor >= visible {
		lm.cursor = visible - 1
	}
	return lm.ensureCursorVisible(visible)
}

func (lm listModel) expandAncestorsOfSelection() listModel {
	if lm.selectedUID == "" {
		return lm
	}
	byKey := make(map[issueKey]Issue, len(lm.issues))
	var selectedKey issueKey
	for _, iss := range lm.issues {
		key := issueKey{projectID: iss.ProjectID, shortID: iss.ShortID}
		byKey[key] = iss
		if iss.UID == lm.selectedUID &&
			(lm.selectedProjectID == 0 || iss.ProjectID == lm.selectedProjectID) {
			selectedKey = key
		}
	}
	if selectedKey.shortID == "" {
		return lm
	}
	seen := map[issueKey]bool{selectedKey: true}
	for key := selectedKey; ; {
		iss := byKey[key]
		if iss.ParentShortID == nil {
			return lm
		}
		parentKey := issueKey{projectID: iss.ProjectID, shortID: *iss.ParentShortID}
		if seen[parentKey] {
			return lm
		}
		if _, ok := byKey[parentKey]; !ok {
			return lm
		}
		if lm.expanded == nil {
			lm.expanded = expansionSet{}
		}
		lm.expanded[parentKey] = true
		seen[parentKey] = true
		key = parentKey
	}
}

// nextStatus cycles "" → "open" → "closed" → "".
func nextStatus(s string) string {
	switch s {
	case "":
		return "open"
	case "open":
		return "closed"
	default:
		return ""
	}
}

// applyFetched stores the latest issue list and restores the cursor
// onto the same issue (by UID) when possible — identity-based
// selection. Issue lists come back sorted by updated_at DESC, so any
// background mutation can shuffle row order under agent churn; pinning
// to the index would silently move the highlight to a different issue.
//
// When the previously-selected issue is no longer visible (filtered
// out, deleted, or scope changed), the cursor falls back to the same
// index clamped to the new visible-row count. Empty list clears both
// cursor and selectedUID.
func (lm listModel) applyFetched(msg tea.Msg) listModel {
	switch m := msg.(type) {
	case initialFetchMsg:
		lm.loading = false
		lm.err = m.err
		if m.err == nil {
			lm.issues, lm.truncated = trimQueueWorkingSet(m.issues)
		}
	case refetchedMsg:
		lm.err = m.err
		if m.err == nil {
			lm.issues, lm.truncated = trimQueueWorkingSet(m.issues)
		}
	}
	lm = lm.expandAncestorsOfSelection()
	rows := lm.visibleRows()
	if len(rows) == 0 {
		lm.cursor = 0
		lm.windowStart = 0
		lm.selectedUID = ""
		lm.selectedProjectID = 0
		return lm
	}
	if lm.selectedUID != "" {
		for i, row := range rows {
			if row.issue.UID == lm.selectedUID &&
				(lm.selectedProjectID == 0 || row.issue.ProjectID == lm.selectedProjectID) {
				lm.cursor = i
				return lm.ensureCursorVisible(len(rows))
			}
		}
	}
	// Selection lost — clamp the prior index into the new visible range
	// and re-record the issue under it so the next refetch tries to
	// follow that one instead.
	if lm.cursor >= len(rows) {
		lm.cursor = len(rows) - 1
	}
	if lm.cursor < 0 {
		lm.cursor = 0
	}
	lm = lm.ensureCursorVisible(len(rows))
	lm.selectedUID = rows[lm.cursor].issue.UID
	lm.selectedProjectID = rows[lm.cursor].issue.ProjectID
	return lm
}

// applyMutation handles a mutationDoneMsg arriving at the list view.
// "create", "close", "reopen" kinds all seed the status line and (on
// success) dispatch a refetch so the row updates without waiting for
// SSE invalidation (Task 11). Mutations whose origin is "detail" are
// dropped here so a detail-side close that completes after the user
// pops back to the list does not steal the list status line; SSE-
// driven invalidation will keep the list cache in sync once Task 11
// lands.
//
// TODO(task-12): replace lm.status string with Model-level toast
// machinery (messages.go::toastExpiredMsg + toast). The status line is
// a placeholder; toasts will own auto-expiry and stacked notifications.
func (lm listModel) applyMutation(
	m mutationDoneMsg, api listAPI, sc scope,
) (listModel, tea.Cmd) {
	if m.origin != "list" {
		return lm, nil
	}
	if m.err != nil {
		lm.status = errorStyle.Render(
			fmt.Sprintf("%s failed: %s", m.kind, m.err.Error()),
		)
		return lm, nil
	}
	lm.status = listMutationSuccessText(m)
	// On a successful create, seed selection with the new issue's
	// UID so applyFetched lands the cursor on the new row after
	// refetch instead of snapping back to whatever was selected
	// before. Recency-sorted lists put the new issue at index 0;
	// without this seed the cursor returns to the prior row and the
	// just-created issue can fall off-screen — roborev #113 finding 2.
	if m.kind == "create" && m.resp != nil && m.resp.Issue != nil {
		lm.selectedUID = m.resp.Issue.UID
		lm.selectedProjectID = projectIDForRow(*m.resp.Issue, sc)
		lm.cursor = 0
	}
	return lm, lm.refetchCmd(api, sc)
}

// listMutationSuccessText is the per-kind status hint after a successful
// mutation. The short_id comes from m.resp.Issue when present; otherwise
// the hint omits it so we don't print "#".
func listMutationSuccessText(m mutationDoneMsg) string {
	ref := ""
	if m.resp != nil && m.resp.Issue != nil {
		ref = m.resp.Issue.ShortID
	}
	switch m.kind {
	case "create":
		return fmt.Sprintf("created #%s", ref)
	case "close":
		return fmt.Sprintf("closed #%s", ref)
	case "reopen":
		return fmt.Sprintf("reopened #%s", ref)
	case "priority.set":
		return fmt.Sprintf("set priority of #%s", ref)
	case "priority.clear":
		return fmt.Sprintf("cleared priority of #%s", ref)
	}
	return ""
}

// dispatchCreateIssue is the create-issue dispatch path, callable
// from any caller that has the listModel in scope. Plan 8 commit 4
// extended the signature with labels + owner so the multi-field
// new-issue form can pass through normalized values; the title is
// sent untrimmed so deliberate leading/trailing whitespace survives.
// Empty/whitespace-only titles short-circuit so accidental Enter is
// a quiet no-op (the form's own validation surfaces "title is
// required" before this is reached, but the gate is kept for any
// future caller that doesn't pre-validate).
func (lm listModel) dispatchCreateIssue(
	api listAPI, sc scope,
	title, body string, labels []string, owner *string,
) (listModel, tea.Cmd) {
	if strings.TrimSpace(title) == "" {
		return lm, nil
	}
	actor := lm.actor
	pid := sc.projectID
	return lm, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.CreateIssue(ctx, pid, CreateIssueBody{
			Title: title, Body: body, Labels: labels, Owner: owner, Actor: actor,
		})
		return mutationDoneMsg{origin: "list", kind: "create", resp: resp, err: err}
	}
}

// refetchCmd returns a tea.Cmd that re-fetches the capped all-status
// issue working set. Render filters narrow the result via filteredIssues
// without changing the wire request.
//
// dispatchKey captures the scope/filter at dispatch time;
// Model.populateCache compares it against the current state and drops
// stale responses so a slow refetch can't overwrite the list after
// the user has changed filter, switched scope, or another refetch
// reordered ahead of it.
func (lm listModel) refetchCmd(api listAPI, sc scope) tea.Cmd {
	filter := queueFetchFilter()
	dispatchKey := cacheKey{
		allProjects: sc.allProjects, projectID: sc.projectID, limit: filter.Limit,
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var (
			issues []Issue
			err    error
		)
		if sc.allProjects {
			issues, err = api.ListAllIssues(ctx, filter)
		} else {
			issues, err = api.ListIssues(ctx, sc.projectID, filter)
		}
		return refetchedMsg{dispatchKey: dispatchKey, issues: issues, err: err}
	}
}

// filteredIssues returns the subset of issues that satisfy the
// client-side filters (Status/Owner/Author/Search/Labels). The fast path
// returns the input slice unchanged when no client-side filter is set
// — render runs every keystroke, so this matters.
//
// Plan 8 commit 5b added the Labels axis: with the wire now carrying
// per-issue labels (api.IssueOut), the early-return condition gained a
// len(f.Labels) == 0 check so a label-only filter no longer
// short-circuits past matchesFilter.
func filteredIssues(issues []Issue, f ListFilter) []Issue {
	if f.Status == "" && f.Owner == "" && f.Author == "" && f.Search == "" && len(f.Labels) == 0 {
		return issues
	}
	out := make([]Issue, 0, len(issues))
	for _, iss := range issues {
		if matchesFilter(iss, f) {
			out = append(out, iss)
		}
	}
	return out
}

// matchesFilter reports whether iss satisfies the client-side filters.
// Owner is *string on the wire, so a nil pointer never matches a set
// owner. Search is case-insensitive contains over Title, ShortID, and
// QualifiedID so users can paste an id (`abc4` or `kata#abc4`) into the
// `/` search and find the issue without having to remember the title.
// Body search is out of scope: the list rows don't carry the body.
//
// Labels use any-of semantics: a non-empty filter Labels slice matches
// when at least one of the filter's labels is attached to the issue. An
// empty filter Labels slice (len == 0) is the no-filter case — every
// issue passes that gate.
func matchesFilter(iss Issue, f ListFilter) bool {
	if f.Status != "" && iss.Status != f.Status {
		return false
	}
	if f.Owner != "" {
		if iss.Owner == nil || *iss.Owner != f.Owner {
			return false
		}
	}
	if f.Author != "" && iss.Author != f.Author {
		return false
	}
	if f.Search != "" && !matchesSearchTerm(iss, f.Search) {
		return false
	}
	if len(f.Labels) > 0 && !labelsAnyOf(iss.Labels, f.Labels) {
		return false
	}
	return true
}

// matchesSearchTerm runs the `/` filter as case-insensitive contains
// across the issue's user-visible identifiers: title, short_id, and
// the qualified `<project>#<short_id>` form. Returning true on any
// match keeps the search forgiving — pasting a bare or qualified id
// surfaces the issue without forcing users to know which form the
// list row currently displays.
func matchesSearchTerm(iss Issue, term string) bool {
	needle := strings.ToLower(term)
	for _, field := range [...]string{iss.Title, iss.ShortID, iss.QualifiedID} {
		if field == "" {
			continue
		}
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

// labelsAnyOf reports whether any of want is present in have. Used by
// matchesFilter to implement the Labels axis' any-of semantics. Empty
// want returns true (no filter) so callers can skip the branch entirely
// when no label filter is configured; both sides empty also returns
// true. Lookup is via a small map so an issue with many attached labels
// doesn't pay an O(n*m) cost.
func labelsAnyOf(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	attached := make(map[string]struct{}, len(have))
	for _, l := range have {
		attached[l] = struct{}{}
	}
	for _, l := range want {
		if _, ok := attached[l]; ok {
			return true
		}
	}
	return false
}

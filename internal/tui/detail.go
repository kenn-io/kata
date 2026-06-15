package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// detailTab names which sub-tab the detail view is rendering.
type detailTab int

const (
	tabComments detailTab = iota
	tabEvents
	tabLinks
)

// detailTabCount is the modulus for the tab cycle.
const detailTabCount = 3

type detailFocus int

const (
	focusActivity detailFocus = iota
	focusChildren
)

// detailNavCap caps the nav stack at 1 prior entry — current + prior =
// 2 levels per the plan's "2-element stack." Jumping from level 2 is
// a no-op; Esc still pops back to level 1.
const detailNavCap = 1

type uidDisplayFormat int

const (
	uidDisplayNone uidDisplayFormat = iota
	uidDisplayShort
	uidDisplayFull
)

// detailAPI is the subset of *Client the detail view needs. Mirrors
// listAPI so detail_test.go can drive Update with a fake. ref is a
// short_id, qualified short_id, or 26-char ULID — the daemon's path
// resolver accepts all three; ListEvents reads the issue_short_id
// embedded in each event row to filter the project-wide poll
// response.
type detailAPI interface {
	GetIssueDetail(ctx context.Context, projectID int64, ref string) (*IssueDetail, error)
	ListComments(ctx context.Context, projectID int64, ref string) ([]CommentEntry, error)
	ListEvents(ctx context.Context, projectID int64, ref string) ([]EventLogEntry, error)
	ListLinks(ctx context.Context, projectID int64, ref string) ([]LinkEntry, error)
	Close(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error)
	Reopen(ctx context.Context, projectID int64, ref, actor string) (*MutationResp, error)
	AddLabel(
		ctx context.Context, projectID int64, ref, label, actor string,
	) (*MutationResp, error)
	RemoveLabel(
		ctx context.Context, projectID int64, ref, label, actor string,
	) (*MutationResp, error)
	Assign(
		ctx context.Context, projectID int64, ref, owner, actor string,
	) (*MutationResp, error)
	SetPriority(
		ctx context.Context, projectID int64, ref string, priority *int64, actor string,
	) (*MutationResp, error)
	AddLink(
		ctx context.Context, projectID int64, ref string, body LinkBody, actor string,
	) (*MutationResp, error)
	EditBody(
		ctx context.Context, projectID int64, ref, body, actor string,
	) (*MutationResp, error)
	AddComment(
		ctx context.Context, projectID int64, ref, body, actor string,
	) (*MutationResp, error)
}

// detailModel owns detail-view state. activeTab + tabCursor address
// the highlighted row; navStack holds the prior detailModel so Esc
// pops back to the issue the user jumped from. scopePID and scope-
// flags carry the project_id used for jump fetches. modal/status/actor
// support the Task 9 mutation path: modal is the inline label/owner/
// link prompt, status is the one-shot toast text (Task 12 will swap to
// timed expiry), and actor is the user identity threaded into mutations.
//
// gen is the detail-open generation: it increments every time the user
// opens or jumps to a different issue. Every async fetch and detail-
// originated mutation captures the current gen at dispatch time, and
// applyFetched/applyMutation discard messages whose gen no longer
// matches so an in-flight response cannot pollute a different issue.
//
// commentsLoading / eventsLoading / linksLoading and their per-tab
// error siblings drive the per-tab placeholders ("(loading...)" /
// "comments: <err>") so the user can tell whether an empty tab is the
// daemon still working or a real failure they should react to.
type detailModel struct {
	issue           *Issue
	parent          *IssueRef
	children        []Issue
	detailFocus     detailFocus
	loading         bool
	err             error
	gen             int64
	activeTab       detailTab
	scroll          int // unified viewport offset in document lines
	tabCursor       int // active-tab row cursor
	childCursor     int
	comments        []CommentEntry
	events          []EventLogEntry
	links           []LinkEntry
	commentsLoading bool
	eventsLoading   bool
	linksLoading    bool
	commentsErr     error
	eventsErr       error
	linksErr        error
	navStack        []detailModel
	scopePID        int64
	allProjects     bool
	actor           string
	status          string
	// tabExplicit tracks whether the user has manually selected the
	// active activity tab. False on a fresh detail open; flipped to
	// true the first time the tab cycler runs. applyFetched uses this
	// to auto-pick the first non-empty activity tab on initial load
	// (so a Comments(0)+Events(N) issue lands on Events) without
	// stomping a user choice when a late-arriving fetch resolves.
	tabExplicit bool
	// lastTermWidth / lastTermHeight cache the most recent terminal
	// dimensions seen by Model.routeTopLevel's WindowSizeMsg handler.
	// viewportDims reads them to compute the visible-row count for
	// page-step sizing and reveal-cursor math, and to clamp dm.scroll
	// against the document's rendered max start so overscrolling past
	// EOF can't let the offset grow unbounded.
	lastTermWidth  int
	lastTermHeight int
	// lastDetail* caches the actual detail viewport when the model can compute
	// it (notably split-pane mode, where the detail pane is narrower/shorter
	// than the terminal). When unset, viewport math falls back to the
	// stacked full-terminal calculation.
	lastDetailWidth  int
	lastDetailHeight int
	lastDetailSplit  bool
	uidFormat        uidDisplayFormat
	// dm.modal was removed in M3b — the M3a/b input infrastructure on
	// Model.input owns inline label/owner/link prompts now.
}

// newDetailModel returns a zeroed detailModel.
func newDetailModel() detailModel { return detailModel{} }

// Update routes detail-view messages: keys and the four fetch results.
// After M3b the dm.modal in-place input was retired; the panel-local
// prompt lives on Model.input and is dispatched at the Model level,
// so dm.Update no longer has a "modal active" branch.
func (dm detailModel) Update(msg tea.Msg, km keymap, api detailAPI) (detailModel, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyMsg:
		return dm.handleKey(m, km, api)
	case mutationDoneMsg:
		return dm.applyMutation(m, api)
	case detailFetchedMsg, commentsFetchedMsg, eventsFetchedMsg, linksFetchedMsg:
		return dm.applyFetched(msg), nil
	}
	return dm, nil
}

// applyFetched seeds dm with the payload from one of the four fetched-
// messages. Per-tab errors land on dm.commentsErr/eventsErr/linksErr
// so the user can tell which tab failed; the detail-issue error still
// rides dm.err because it gates the entire view. Messages whose gen
// does not match dm.gen are dropped so an in-flight fetch cannot
// pollute a different issue after Esc + reopen or an Enter-jump to a
// referenced issue.
//
// After per-tab payloads land, autoSelectActivityTab promotes the
// active tab to the first non-empty one — but only when the user has
// not yet picked a tab manually (dm.tabExplicit). This keeps the
// initial-load Comments(0) → Events(N) jump from pulling focus away
// once the user has cycled tabs themselves.
func (dm detailModel) applyFetched(msg tea.Msg) detailModel {
	switch m := msg.(type) {
	case detailFetchedMsg:
		if m.gen != dm.gen {
			return dm
		}
		dm.loading = false
		if m.issue != nil {
			dm.issue = m.issue
		}
		dm.parent = m.parent
		dm.children = m.children
		dm = dm.clampChildFocus()
		dm.err = mergeErr(dm.err, m.err)
	case commentsFetchedMsg:
		if m.gen != dm.gen {
			return dm
		}
		dm.commentsLoading = false
		dm.comments = m.comments
		dm.commentsErr = m.err
		dm = dm.autoSelectActivityTab()
	case eventsFetchedMsg:
		if m.gen != dm.gen {
			return dm
		}
		dm.eventsLoading = false
		dm.events = m.events
		dm.eventsErr = m.err
		dm = dm.autoSelectActivityTab()
	case linksFetchedMsg:
		if m.gen != dm.gen {
			return dm
		}
		dm.linksLoading = false
		dm.links = m.links
		dm.linksErr = m.err
		dm = dm.autoSelectActivityTab()
	}
	return dm
}

// autoSelectActivityTab picks the first non-empty activity tab the
// first time fetches resolve, in order Comments → Events → Links. It
// is a no-op once the user has cycled tabs (dm.tabExplicit) so an
// in-flight fetch cannot yank focus off the user's choice. When all
// tabs are empty the active tab keeps its default so the placeholder
// strip (`[ Comments (0) ]`) still reads naturally. The tabCursor
// resets only when the active tab actually changes — an unrelated
// late fetch (e.g., links arriving after the user already scrolled
// the comments tab) must not pull the cursor back to the top
// (roborev #17155 finding 3).
func (dm detailModel) autoSelectActivityTab() detailModel {
	if dm.tabExplicit {
		return dm
	}
	prev := dm.activeTab
	switch {
	case len(dm.comments) > 0:
		dm.activeTab = tabComments
	case len(dm.events) > 0:
		dm.activeTab = tabEvents
	case len(dm.links) > 0:
		dm.activeTab = tabLinks
	}
	if dm.activeTab != prev {
		dm.tabCursor = 0
	}
	return dm
}

// mergeErr keeps the last non-nil error so a successful fetch on one
// tab does not clear an earlier failure on another. Today only the
// detail-issue fetch path uses this; the per-tab errors live on their
// own dm fields.
func mergeErr(prev, next error) error {
	if next != nil {
		return next
	}
	return prev
}

// handleKey dispatches detail bindings. The function is intentionally a
// thin router: navigation keys live here directly, mutation keys defer
// to handleMutationKey so the cyclomatic budget (≤8) holds.
func (dm detailModel) handleKey(
	msg tea.KeyMsg, km keymap, api detailAPI,
) (detailModel, tea.Cmd) {
	if next, cmd, ok := dm.handleNavKey(msg, km, api); ok {
		return next, cmd
	}
	if next, cmd, ok := dm.handleMutationKey(msg, km, api); ok {
		return next, cmd
	}
	return dm, nil
}

// handleNavKey processes the navigation/cursor/scroll/tab bindings.
// Returns ok=true when the key was consumed; handleKey forwards to
// handleMutationKey otherwise.
//
// PageUp/PageDown are the dedicated body-scroll keys: they always
// move dm.scroll regardless of focus, so a long issue body can be
// skimmed even while the cursor is on a populated activity tab or
// the children list. j/k stays bound to the focused section's cursor
// (tab cursor in activity, child cursor in children) — without a
// dedicated key the body scroll path was unreachable in practice
// because the events tab almost always has at least one row.
func (dm detailModel) handleNavKey(
	msg tea.KeyMsg, km keymap, api detailAPI,
) (detailModel, tea.Cmd, bool) {
	switch {
	case km.NextTab.matches(msg):
		return dm.cycleDetailFocus(1).revealCursor(), nil, true
	case km.PrevTab.matches(msg):
		return dm.cycleDetailFocus(-1).revealCursor(), nil, true
	case km.PageUp.matches(msg):
		return dm.pageScrollUp(), nil, true
	case km.PageDown.matches(msg):
		return dm.pageScrollDown(), nil, true
	case km.ScrollUp.matches(msg):
		return dm.scrollViewportBy(-1), nil, true
	case km.ScrollDown.matches(msg):
		return dm.scrollViewportBy(1), nil, true
	case km.Up.matches(msg):
		next, moved := dm.handleUp()
		if moved {
			next = next.revealCursor()
		}
		return next, nil, true
	case km.Down.matches(msg):
		next, moved := dm.handleDown()
		if moved {
			next = next.revealCursor()
		}
		return next, nil, true
	case km.Open.matches(msg):
		next, cmd := dm.handleEnter(api)
		return next, cmd, true
	case km.Back.matches(msg):
		next, cmd := dm.handleBack()
		return next, cmd, true
	default:
		return dm, nil, false
	}
}

// scrollViewportBy slides the detail viewport `delta` rows. Negative
// scrolls up, positive down. The result is clamped to [0, maxStart]
// against the unified document the renderer will draw.
func (dm detailModel) scrollViewportBy(delta int) detailModel {
	width, visible, ok := dm.viewportDims()
	dm.scroll += delta
	if dm.scroll < 0 {
		dm.scroll = 0
	}
	if !ok {
		return dm
	}
	docLines, _ := dm.detailDocumentLines(width, dm.scrollChrome())
	if maxStart := viewportMaxStart(len(docLines), visible); dm.scroll > maxStart {
		dm.scroll = maxStart
	}
	return dm
}

// pageScrollUp / pageScrollDown step by one viewport-minus-overlap so
// PgUp/PgDn jump a page while keeping a couple of rows of context
// across the seam. Falls back to a fixed step when terminal dimensions
// haven't been observed yet (cold start before the first WindowSizeMsg).
func (dm detailModel) pageScrollUp() detailModel {
	return dm.scrollViewportBy(-dm.pageStep())
}

func (dm detailModel) pageScrollDown() detailModel {
	return dm.scrollViewportBy(dm.pageStep())
}

// detailPageOverlap is the row count kept visible across a page step
// so the user retains context. Same intent as a "less"-style page key.
const detailPageOverlap = 2

// detailFallbackPageStep is the page size used when terminal
// dimensions are unknown (initial dispatch before WindowSizeMsg).
const detailFallbackPageStep = 8

func (dm detailModel) pageStep() int {
	_, visible, ok := dm.viewportDims()
	if !ok || visible <= detailPageOverlap {
		return detailFallbackPageStep
	}
	return visible - detailPageOverlap
}

// viewportDims returns the width and visible-row count of the detail
// document viewport using the most recent terminal dimensions seen by
// Update. In split mode the cached lastDetailWidth/Height already
// record the pane's inner area; in stacked mode visible rows equal
// terminal height minus the title bar, info line, and footer.
func (dm detailModel) viewportDims() (width, visible int, ok bool) {
	if dm.lastDetailSplit && dm.lastDetailWidth > 0 && dm.lastDetailHeight > 0 {
		return dm.lastDetailWidth, dm.lastDetailHeight, true
	}
	if dm.lastTermWidth <= 0 || dm.lastTermHeight <= 0 {
		return 0, 0, false
	}
	helpRows := detailHelpRows(dm, dm.scrollChrome())
	footerLines := helpLines(helpRows, dm.lastTermWidth)
	visible = dm.lastTermHeight - 2 - 1 - footerLines
	if visible < 1 {
		visible = 1
	}
	return dm.lastTermWidth, visible, true
}

// scrollChrome returns the chrome subset that affects document line
// count (notably the all-projects metadata row). It carries the same
// scope identity the View call sees so scroll math doesn't drift from
// the rendered layout.
func (dm detailModel) scrollChrome() viewChrome {
	return viewChrome{scope: scope{
		allProjects: dm.allProjects,
		projectID:   dm.scopePID,
	}}
}

// revealCursor scrolls the viewport so the focused section's cursor
// (children or active activity tab) is visible. A no-op when the
// cursor is already in the window or terminal dims aren't known yet.
// Called after Tab/section-cursor moves so the user always sees the
// row they're acting on.
func (dm detailModel) revealCursor() detailModel {
	width, visible, ok := dm.viewportDims()
	if !ok {
		return dm
	}
	docLines, anchors := dm.detailDocumentLines(width, dm.scrollChrome())
	row := -1
	switch dm.detailFocus {
	case focusChildren:
		row = anchors.childCursor
	case focusActivity:
		row = anchors.tabCursor
		// When the active tab has no entries yet (loading / error /
		// empty placeholder), pin the activity header to the top of
		// the viewport so the user knows where focus landed.
		if row < 0 && anchors.activity >= 0 {
			row = anchors.activity - 1
		}
	}
	if row < 0 {
		return dm
	}
	dm.scroll = scrollToReveal(dm.scroll, row, visible, len(docLines))
	return dm
}

// handleUp moves the section cursor (children or active activity tab)
// up by one. When there is no section cursor — no children and no
// activity rows — j/k spills to viewport scroll so the binding never
// feels dead. The bool return reports whether a section cursor moved
// (or stayed put on a populated section); false means the call
// scrolled the viewport instead. handleNavKey uses that signal to
// skip revealCursor on the scroll path — otherwise revealCursor would
// pin the activity header back into view and undo the scroll.
// ↑/↓ have their own scroll handlers wired in handleNavKey.
func (dm detailModel) handleUp() (detailModel, bool) {
	if dm.detailFocus == focusChildren {
		if dm.childCursor > 0 {
			dm.childCursor--
		}
		return dm, true
	}
	if dm.activeRowCount() > 0 {
		if dm.tabCursor > 0 {
			dm.tabCursor--
		}
		return dm, true
	}
	return dm.scrollViewportBy(-1), false
}

// handleDown moves the section cursor down by one. Clamps at the last
// row; spills to viewport scroll when there is no section cursor —
// see handleUp for the rationale and the bool return contract.
func (dm detailModel) handleDown() (detailModel, bool) {
	if dm.detailFocus == focusChildren {
		if dm.childCursor < len(dm.children)-1 {
			dm.childCursor++
		}
		return dm, true
	}
	if n := dm.activeRowCount(); n > 0 {
		if dm.tabCursor < n-1 {
			dm.tabCursor++
		}
		return dm, true
	}
	return dm.scrollViewportBy(1), false
}

// handleBack pops one level off the nav stack if non-empty, otherwise
// returns popDetailCmd so the top-level Model reverts to viewList.
func (dm detailModel) handleBack() (detailModel, tea.Cmd) {
	if len(dm.navStack) == 0 {
		return dm, popDetailCmd()
	}
	prev := dm.navStack[len(dm.navStack)-1]
	prev.navStack = dm.navStack[:len(dm.navStack)-1]
	return prev, nil
}

// handleEnter dispatches the Enter-jump on events/links tabs. The
// comments tab does not navigate. No-op when the api is unwired, the
// stack is at cap, or there is no jump target under the cursor.
//
// Rather than building the new detailModel here, handleEnter emits
// jumpDetailMsg so the top-level Model can allocate the new gen from
// its monotonic m.nextGen counter — see Model.handleJumpDetail. dm is
// returned unchanged because the navStack push and gen advance happen
// at the Model level when it consumes jumpDetailMsg.
func (dm detailModel) handleEnter(api detailAPI) (detailModel, tea.Cmd) {
	if api == nil || len(dm.navStack) >= detailNavCap {
		return dm, nil
	}
	if dm.detailFocus == focusChildren {
		if dm.childCursor < 0 || dm.childCursor >= len(dm.children) {
			return dm, nil
		}
		// A child carries its own numeric project_id (hydrated
		// cross-project on the show response), so route the jump to that
		// project — a child in another project must not resolve under the
		// current parent's project.
		child := dm.children[dm.childCursor]
		return dm, jumpDetailCmd(jumpTargetRef{projectID: child.ProjectID, ref: child.ShortID})
	}
	target, ok := dm.jumpTarget()
	if !ok {
		return dm, nil
	}
	return dm, jumpDetailCmd(target)
}

type detailFocusSlot struct {
	focus detailFocus
	tab   detailTab
}

func (dm detailModel) cycleDetailFocus(delta int) detailModel {
	slots := dm.detailFocusSlots()
	if len(slots) == 0 {
		return dm
	}
	idx := dm.currentFocusSlotIndex(slots)
	idx = (idx + delta + len(slots)) % len(slots)
	slot := slots[idx]
	dm.detailFocus = slot.focus
	if slot.focus == focusActivity {
		dm.activeTab = slot.tab
		dm.tabCursor = 0
		dm.tabExplicit = true
	} else {
		dm.childCursor = clampInt(dm.childCursor, 0, len(dm.children)-1)
	}
	return dm
}

// detailFocusSlots enumerates the focus stops Tab can land on. Children
// slot only appears when there are children; activity slots only appear
// when the activity section is rendered (hasActivity() — covers loaded
// data, in-flight fetches, and errors). Without this gate, a children-
// only issue could Tab into invisible activity states and the cursor
// would silently disappear (roborev #17131 finding 4).
func (dm detailModel) detailFocusSlots() []detailFocusSlot {
	slots := make([]detailFocusSlot, 0, 4)
	if len(dm.children) > 0 {
		slots = append(slots, detailFocusSlot{focus: focusChildren})
	}
	if dm.hasActivity() {
		slots = append(slots,
			detailFocusSlot{focus: focusActivity, tab: tabComments},
			detailFocusSlot{focus: focusActivity, tab: tabEvents},
			detailFocusSlot{focus: focusActivity, tab: tabLinks},
		)
	}
	return slots
}

func (dm detailModel) currentFocusSlotIndex(slots []detailFocusSlot) int {
	for i, slot := range slots {
		if slot.focus == focusChildren && dm.detailFocus == focusChildren {
			return i
		}
		if slot.focus == focusActivity && dm.detailFocus == focusActivity && slot.tab == dm.activeTab {
			return i
		}
	}
	return 0
}

func (dm detailModel) clampChildFocus() detailModel {
	if len(dm.children) == 0 {
		dm.childCursor = 0
		if dm.detailFocus == focusChildren {
			dm.detailFocus = focusActivity
		}
		return dm
	}
	dm.childCursor = clampInt(dm.childCursor, 0, len(dm.children)-1)
	return dm
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// jumpDetailCmd emits a jumpDetailMsg so Model.handleJumpDetail can
// perform the actual jump (with a fresh monotonic gen). Splitting the
// emit off handleEnter keeps the cmd shape obvious in tests.
func jumpDetailCmd(target jumpTargetRef) tea.Cmd {
	return func() tea.Msg {
		return jumpDetailMsg{
			ref:         target.ref,
			projectID:   target.projectID,
			projectName: target.projectName,
		}
	}
}

// jumpTargetRef identifies a jump destination: the project the target
// lives in (projectID for children, projectName for link peers; both zero
// / empty means "the current detail project") and the ref to resolve
// under that project.
type jumpTargetRef struct {
	projectID   int64
	projectName string
	ref         string
}

// jumpTarget returns the destination to jump to from the active tab +
// cursor. Comments tab never jumps. Events tab reads the payload's peer
// short_id (to_short_id / issue_short_id), which is a current-project ref.
// Links tab reads the link's To peer, or the From peer when the link is
// incoming (the link's To side is the current issue) so Enter takes the
// user to the other end of the relation — carrying that peer's project so
// a cross-project link resolves under the peer's own project.
func (dm detailModel) jumpTarget() (jumpTargetRef, bool) {
	current := ""
	currentUID := ""
	if dm.issue != nil {
		current = dm.issue.ShortID
		currentUID = dm.issue.UID
	}
	switch dm.activeTab {
	case tabEvents:
		target, ok := eventJumpTarget(dm.events, dm.tabCursor)
		if !ok || target == "" || (current != "" && target == current) {
			return jumpTargetRef{}, false
		}
		return jumpTargetRef{ref: target}, true
	case tabLinks:
		return linkJumpTarget(dm.links, dm.tabCursor, current, currentUID)
	}
	return jumpTargetRef{}, false
}

// activeRowCount is the row count for the active tab.
func (dm detailModel) activeRowCount() int {
	switch dm.activeTab {
	case tabComments:
		return len(dm.comments)
	case tabEvents:
		return len(dm.events)
	case tabLinks:
		return len(dm.links)
	}
	return 0
}

// activeChunks returns the chunk list for the current active tab,
// dispatching to the per-tab chunk builders so the chunks ARE the
// renderable content (cursor markers and styled headers included).
// detailDocumentLines uses these directly — eliminating the second
// markdown pass that the previous implementation paid via
// renderActiveTabFull on top of activeChunks.
//
// width is the rendered width of the tab pane (the same width passed
// to the per-tab renderers).
func (dm detailModel) activeChunks(width int) []entryChunk {
	switch dm.activeTab {
	case tabComments:
		return commentChunks(dm.comments, width, dm.tabCursor,
			tabState{loading: dm.commentsLoading, err: dm.commentsErr})
	case tabEvents:
		return eventChunks(dm.events, width, dm.tabCursor,
			tabState{loading: dm.eventsLoading, err: dm.eventsErr})
	case tabLinks:
		return linkChunks(dm.links, width, dm.tabCursor,
			tabState{loading: dm.linksLoading, err: dm.linksErr})
	}
	return nil
}

// popDetailCmd emits popDetailMsg so the top-level Model reverts to
// viewList. listModel is held by value so its cursor and filter state
// survive the round trip untouched.
func popDetailCmd() tea.Cmd {
	return func() tea.Msg { return popDetailMsg{} }
}

// detailProjectID picks the project_id to fetch under. In all-projects
// scope, we use issue.ProjectID (the issue carries it on the wire); in
// single-project scope we use sc.projectID. Zero issue.ProjectID falls
// back to sc so fixtures that omit ProjectID still work.
func detailProjectID(iss Issue, sc scope) int64 {
	if sc.allProjects && iss.ProjectID != 0 {
		return iss.ProjectID
	}
	return sc.projectID
}

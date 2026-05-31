package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type viewID int

const (
	viewList viewID = iota
	viewDetail
	viewHelp
	viewEmpty
	viewProjects
	viewDaemons
)

// Model is the top-level Bubble Tea model. Sub-views are embedded by
// value so Update can mutate them in place without indirection. The
// detail sub-view is held by value (not pointer) so its scroll/tab
// state lives across opens of the same issue, and so popDetailMsg
// returns to a list whose cursor and filters are unchanged.
//
// SSE state lives on the parent model so the consumer goroutine has a
// fixed channel to push into and the detail/list sub-views can route
// invalidation. sseCh bridges the long-lived goroutine into the TEA
// loop via waitForSSE; sseStatus drives the status-bar reconnect
// indicator; pendingRefetch coalesces bursts of events into a single
// 150ms-debounced list refetch; cache holds the current list snapshot
// so a stale-mark + clean refetch can short-circuit redundant work.
//
// toastNow is a clock injection point: production uses time.Now, tests
// replace it to drive deterministic toast expiry.
type Model struct {
	opts                Options
	api                 KataAPI
	scope               scope
	view                viewID
	prevView            viewID
	width               int
	height              int
	keymap              keymap
	list                listModel
	detail              detailModel
	sseCh               chan tea.Msg
	sseStatus           sseConnState
	pendingRefetch      bool
	connGen             uint64
	daemonSwitchAttempt uint64
	sseRestart          func(daemonConnection, uint64, chan tea.Msg) tea.Cmd
	// projectsStale flags that the projects table needs a refetch. Set by
	// the SSE event router when an event's project_id matches a row in
	// m.projectsByID and viewProjects is the active view. Cleared when the
	// debounced fetchProjectsWithStats lands. Spec §6.3.
	projectsStale bool
	// projectsGen increments on every stale-flip from an SSE event. The
	// fetchProjectsWithStats cmd captures the generation at dispatch time
	// so the projectsLoadedMsg handler can detect whether a newer event
	// arrived while the response was in flight, and only clear stale when
	// the response covers the latest generation. Spec §6.3.
	projectsGen uint64
	// projectsRefetchPending coalesces stale-flips inside the 500ms window
	// so a burst of SSE events produces exactly one refetch.
	projectsRefetchPending bool
	cache                  *issueCache
	toast                  *toast
	toastNow               func() time.Time
	// nextGen is the monotonic detail-open generation counter. Every
	// open or jump allocates a fresh value via ++ so a fetch in flight
	// from a previously-jumped issue cannot match a newly-opened issue
	// that happens to occupy the smaller-gen snapshot's place after a
	// handleBack restoration. Detail-side fetches and mutations carry
	// the gen at dispatch time; applyFetched/applyMutation drop
	// messages whose gen no longer matches dm.gen.
	nextGen int64
	// input is the active inline command bar / panel-local prompt /
	// centered form. inputNone means no input is open and global keys
	// route normally. While an input is open, all non-Quit keys go to
	// the input's bubbles model; canQuit() gates global keys.
	input inputState
	// modal is the active centered confirm/info overlay (M3.5b: the
	// quit-confirm modal; future plans add delete-confirm etc.).
	// modalNone is the quiescent state. While a modal is open it
	// owns key dispatch — `y`/`n`/`esc` route through it instead of
	// reaching list/detail handlers.
	modal modalKind
	// nextFormGen is the monotonic centered-form ID counter. Every
	// open of an M4 centered form (body editor / comment) allocates
	// a fresh value via ++. The form's formGen rides with the
	// $EDITOR handoff so a stale editorReturnedMsg arriving after
	// the form was closed (or re-opened against a different issue)
	// is rejected before its content can land in a different form's
	// textarea.
	nextFormGen int64
	// projectLabels caches per-project label aggregates feeding the
	// `+` suggestion menu. Each entry carries its own dispatch
	// generation so a slow ListLabels response can't clobber a newer
	// dispatch's state — see label_cache.go for the gen-stamping
	// rationale. SSE issue.labeled / issue.unlabeled events
	// invalidate per-project entries so the menu re-fetches when the
	// daemon writes new label rows.
	projectLabels *labelCache
	// nextLabelsGen is the monotonic counter behind projectLabels.
	// Bumped at dispatch time (NOT at response time) so the cache's
	// entry.gen is the load-bearing identifier for staleness — the
	// HTTP request reads the just-bumped value, the response carries
	// it back, and handleLabelsFetched compares msg.gen >= entry.gen
	// to decide whether to apply the result.
	nextLabelsGen int64
	// projectsByID maps project_id → display name for the all-projects
	// list view. Populated at boot and after project.* SSE events so
	// list rows in all-projects scope can prefix the title with the
	// owning project's name. Stays nil-safe — a missing entry renders
	// as a numeric "[#N]" prefix instead of crashing.
	projectsByID map[int64]string
	// projectStats is the per-project aggregate cache populated by
	// fetchProjectsWithStats. nil-safe: viewProjects renders rows for
	// projects in projectsByID even if their stats haven't loaded yet.
	projectStats map[int64]ProjectStatsSummary
	// projectIdentByID is retained as a per-project auxiliary label map for the
	// projects view. It currently mirrors project names for compatibility with
	// existing view plumbing.
	projectIdentByID map[int64]string
	// projectsCursor is the highlighted row in viewProjects. Reset when
	// transitioning into the view; preserved across re-renders.
	projectsCursor int
	activeDaemon   daemonTarget
	daemonTargets  []daemonTarget
	daemonCursor   int
	// layout is the EFFECTIVE rendered layout — what the View functions
	// actually draw. Re-evaluated on every WindowSizeMsg via
	// resolveLayout, which consults preferredLayout + layoutLocked +
	// canRenderSplit. See layout.go.
	layout layoutMode
	// preferredLayout is the user's stated intent (set on every
	// toggleLayout). Only consulted when layoutLocked is true; without
	// the lock, layout follows pickLayout's auto-pick. Tracked
	// separately from m.layout so a transient narrow resize that
	// degrades a locked split to stacked does NOT erase the split
	// preference — once the terminal is wide enough again, layout
	// returns to split (roborev #17173 finding 1).
	preferredLayout layoutMode
	// layoutLocked is set when the user explicitly toggles the layout
	// via the ToggleLayout key (default: L). While locked, WindowSizeMsg
	// honors preferredLayout instead of re-running pickLayout —
	// except that an outright too-narrow terminal still degrades to
	// stacked so split never renders unusable UI.
	layoutLocked bool
	// focus names which pane owns key dispatch in split layout. In
	// stacked layout m.view is authoritative; m.focus is only
	// consulted when m.layout == layoutSplit. Default focusList.
	focus focusPane
	// nextDetailFollowGen is the monotonic generation counter behind
	// the M6 split-mode detail-follows-cursor debounce. Every cursor
	// change in the list pane bumps this gen; the 75ms-debounced
	// detailFollowTickMsg carries the gen at dispatch time so a
	// stale tick (one whose gen < the current value) drops cleanly
	// without firing a fetch the user no longer wants.
	nextDetailFollowGen int64
	uidFormat           uidDisplayFormat
}

// initialModel constructs the root Bubble Tea model. Style vars are
// populated against opts.Stdout (or os.Stdout when nil) so unit tests
// that bypass Run still see live styles. Run re-runs applyDefaultColorMode
// once it has the opts.Stdout to pin color detection to the real stream.
//
// sseCh is allocated buffered (16) so a brief stall in Update does not
// block the SSE goroutine on its forwardFrame send. cache is allocated
// here rather than on first event so the SSE-driven invalidation never
// has to nil-check it.
func initialModel(opts Options) Model {
	applyDefaultColorMode(opts.Stdout)
	lm := newListModel()
	lm.actor = resolveTUIActor()
	uidFormat := parseUIDDisplayFormat(opts.DisplayUIDFormat)
	return Model{
		opts:             opts,
		view:             viewList,
		keymap:           newKeymap(),
		list:             lm,
		detail:           newDetailModel(),
		sseCh:            make(chan tea.Msg, 16),
		sseStatus:        sseConnected,
		cache:            newIssueCache(),
		toastNow:         time.Now,
		projectLabels:    newLabelCache(),
		projectsByID:     map[int64]string{},
		projectStats:     map[int64]ProjectStatsSummary{},
		projectIdentByID: map[int64]string{},
		layout:           layoutStacked,
		focus:            focusList,
		uidFormat:        uidFormat,
	}
}

// resolveTUIActor mirrors cmd/kata's actor precedence (env → fallback)
// minus the --as flag and git fallback: the TUI has no flag plumbing
// here and we keep the dependency surface small. Tasks 9/10 re-evaluate
// once the broader mutation path lands and may add a git fallback.
func resolveTUIActor() string {
	if v := os.Getenv("KATA_AUTHOR"); v != "" {
		return v
	}
	return "anonymous"
}

// Init dispatches the initial fetch unless boot landed on the empty
// state or no client is wired (the latter happens in unit tests that
// drive the model directly via teatest.NewTestModel and feed
// initialFetchMsg by hand). The list view sets loading=true at
// construction so the spinner shows until initialFetchMsg arrives.
//
// waitForSSE is registered alongside fetchInitial so the SSE goroutine
// (spawned by Run after this Init returns) has a reader the moment its
// first frame is ready. The reader is replenished on every SSE message
// in Update so the channel is continuously drained.
func (m Model) Init() tea.Cmd {
	// EnableBracketedPaste makes multi-rune pastes arrive as a single
	// KeyMsg the textinput can ingest atomically (via its own
	// Sanitize). Without it, every rune comes through as a separate
	// keystroke — slow visible characters and any newline in the
	// clipboard prematurely fires Enter on the inline new-issue row /
	// command bars.
	if m.view == viewEmpty || m.api == nil {
		return tea.Batch(tea.EnableBracketedPaste, m.waitForSSE())
	}
	if m.view == viewProjects {
		// Boot landed on viewProjects (cwd unresolved + ≥1 project). The
		// stats cache is already seeded by buildRunModel; skip fetchInitial
		// (which would hit /api/v1/projects/0/issues with empty scope) and
		// just drive SSE plus the projects-stats refetch on next event.
		return tea.Batch(tea.EnableBracketedPaste, m.fetchProjects(), m.waitForSSE())
	}
	return tea.Batch(tea.EnableBracketedPaste, m.fetchInitial(), m.fetchProjects(), m.waitForSSE())
}

// waitForSSE is the bridge from the SSE goroutine into the TEA loop. It
// returns a tea.Cmd that blocks on the next message in m.sseCh. tea.Cmds
// are one-shot, so every Update branch that consumes an SSE message
// returns waitForSSE() again to keep the bridge alive. A nil sseCh
// (zeroed Model in tests that bypass initialModel) is treated as a
// terminating bridge so unit tests don't deadlock waiting on a channel
// that will never see a write.
func (m Model) waitForSSE() tea.Cmd {
	if m.sseCh == nil {
		return nil
	}
	ch := m.sseCh
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// fetchProjects loads /api/v1/projects into projectsByID so the
// all-projects list view can label each row with its owning project.
// Errors degrade gracefully — rows render with a numeric "[#N]" prefix
// when the map is missing the entry.
func (m Model) fetchProjects() tea.Cmd {
	api := m.api
	connGen := m.connGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		summaries, err := api.ListProjects(ctx)
		if err != nil {
			return projectsLoadedMsg{connGen: connGen, err: err}
		}
		out := make(map[int64]string, len(summaries))
		for _, p := range summaries {
			out[p.ID] = p.Name
		}
		return projectsLoadedMsg{connGen: connGen, projects: out}
	}
}

// fetchInitial returns a command that issues the first list fetch. The
// scope drives whether this is single-project or cross-project. The
// 5s ceiling matches the daemon's typical p95 list latency.
//
// dispatchKey captures the scope/filter the request was sent under;
// populateCache drops the response if the user has changed scope or
// filter since dispatch so a slow initial fetch can't clobber a fresh
// post-toggle list.
func (m Model) fetchInitial() tea.Cmd {
	api, sc, filter := m.api, m.scope, queueFetchFilter()
	connGen := m.connGen
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
		return initialFetchMsg{connGen: connGen, dispatchKey: dispatchKey, issues: issues, err: err}
	}
}

// initialFilter projects opts into the ListFilter the boot fetch uses.
// Today there is nothing on Options that drives the boot filter, but
// keeping this seam means future tasks can add it without re-shaping
// fetchInitial. The wire request itself only carries Status because the
// daemon's ListIssuesRequest accepts {status, limit} and nothing else.
func initialFilter(_ Options) ListFilter {
	return ListFilter{}
}

// Update routes messages to the active sub-view. Quit is handled at the
// top level so it works from every view, EXCEPT while a list-view inline
// prompt or a detail-view modal is active: typing 'q' into a prompt or
// modal must reach the buffer instead of quitting. The same gate applies
// to ?, R, and any future global key.
//
// openDetailMsg / popDetailMsg are intercepted before the per-view
// dispatch because the view switch lives at this level. The detail
// sub-model is reset on open so a new issue starts at scroll=0 with the
// comments tab — but the list sub-model is untouched on pop, preserving
// the user's cursor and filter state across the round trip.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if next, cmd, ok := m.routeTopLevel(msg); ok {
		return next, cmd
	}
	if next, cmd, ok := m.routeSSE(msg); ok {
		return next, cmd
	}
	var bootstrapCmd tea.Cmd
	switch msg.(type) {
	case initialFetchMsg, refetchedMsg:
		if m.staleConnResult(msg) {
			return m, nil
		}
		if m.isStaleListFetch(msg) {
			return m, nil
		}
		m = m.populateCache(msg)
		if _, isInitial := msg.(initialFetchMsg); isInitial {
			m, bootstrapCmd = m.maybeBootstrapSplitDetail()
		}
	}
	if mut, ok := msg.(mutationDoneMsg); ok {
		if m.staleConnMsg(mut.connGen) {
			return m, nil
		}
		next, cmd := m.routeMutation(mut)
		return next, cmd
	}
	// Editor returns from a centered form's ctrl+e handoff land here
	// before dispatchToView so the writeback can hit m.input. formGen=0
	// (legacy detail-side shell-out) falls through to detail.Update.
	if er, ok := msg.(editorReturnedMsg); ok && er.formGen != 0 {
		next, cmd := m.routeEditorReturn(er)
		return next, cmd
	}
	// Label-cache responses route back into the cache before any view
	// gets a shot at them. The handler is pure state — no command —
	// so we fall through to dispatchToView for any unhandled message
	// shape after.
	if lf, ok := msg.(labelsFetchedMsg); ok {
		if m.staleConnMsg(lf.connGen) {
			return m, nil
		}
		m = m.handleLabelsFetched(lf)
		return m, nil
	}
	if pl, ok := msg.(projectsLoadedMsg); ok {
		if m.staleConnMsg(pl.connGen) {
			return m, nil
		}
		// Drop stale responses entirely (success OR error). A pre-
		// invalidation fetch (older gen) must not overwrite maps already
		// updated by a newer in-flight or completed fetch, AND its error
		// must not surface as a toast over fresh data — otherwise the
		// table reverts to stale data or the user sees a misleading
		// "failed to load" while the visible table is current. Only
		// newer fetches (gen > current, impossible) or current-gen
		// fetches apply. Spec §6.3.
		if pl.gen != m.projectsGen {
			return m, nil
		}
		if pl.err != nil {
			m.toast = &toast{
				text:      "failed to load projects: " + pl.err.Error(),
				level:     toastError,
				expiresAt: m.toastNow().Add(toastNoBindingTTL),
			}
			return m, toastExpireCmd(toastNoBindingTTL)
		}
		if pl.projects != nil {
			m.projectsByID = pl.projects
		}
		if pl.idents != nil {
			m.projectIdentByID = pl.idents
		}
		if pl.stats != nil {
			m.projectStats = pl.stats
			// Stats successfully landed for the current generation;
			// consume the stale flag so a debounce timer already in
			// flight doesn't trigger a redundant refetch. A failed fetch
			// (pl.err != nil above) returns earlier and preserves the
			// flag.
			m.projectsStale = false
		}
		// A shrinking refetch (e.g. archive on another client) may leave
		// m.projectsCursor past the end of the row list. Clamp here so
		// Enter on the visually-highlighted row doesn't no-op against an
		// out-of-range cursor in applyProjectsViewSelection.
		rowCount := len(projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats))
		if m.projectsCursor >= rowCount {
			m.projectsCursor = rowCount - 1
		}
		if m.projectsCursor < 0 {
			m.projectsCursor = 0
		}
		return m, nil
	}
	if _, ok := msg.(projectsDebounceFireMsg); ok {
		m.projectsRefetchPending = false
		if m.view == viewProjects && m.projectsStale {
			return m, m.fetchProjectsWithStats()
		}
		return m, nil
	}
	if sw, ok := msg.(daemonSwitchResultMsg); ok {
		next, cmd := m.handleDaemonSwitchResult(sw)
		return next, cmd
	}
	next, cmd := m.dispatchToView(msg)
	if bootstrapCmd == nil {
		return next, cmd
	}
	if cmd == nil {
		return next, bootstrapCmd
	}
	return next, tea.Batch(cmd, bootstrapCmd)
}

// maybeBootstrapSplitDetail auto-loads the highlighted list row into the
// detail pane after the initial fetch lands in split layout with no
// detail issue pinned yet. scheduleDetailFollow only fires from
// cursor-motion otherwise, so without this hook the right-hand pane
// stays empty on launch until the user nudges j/k.
func (m Model) maybeBootstrapSplitDetail() (Model, tea.Cmd) {
	if m.layout != layoutSplit {
		return m, nil
	}
	if m.detail.issue != nil {
		return m, nil
	}
	return m.scheduleDetailFollow()
}

// isStaleListFetch reports whether a list-fetch message was dispatched
// under a scope/filter that no longer matches the current state. Stale
// fetches are dropped before reaching populateCache or dispatchToView
// so the cache/list aren't churned by a slow reply that the user has
// already moved past.
func (m Model) isStaleListFetch(msg tea.Msg) bool {
	dispatchKey, _, _ := fetchPayload(msg)
	return !cacheKeysEqual(dispatchKey, m.currentCacheKey())
}

// routeMutation dispatches a mutationDoneMsg to the view that
// originated the mutation, with a gen-aware path for detail
// completions that arrive after the user opened a different issue.
//
// Three cases:
//
//  1. origin=list, view!=viewList → apply directly to listModel so
//     the list status/refetch fires even though the user is in
//     detail view now.
//  2. origin=detail, view!=viewDetail → apply directly to dm; its
//     gen is unchanged (no new open since pop) so applyMutation
//     accepts the message.
//  3. origin=detail, view==viewDetail, mut.gen != m.detail.gen →
//     the user opened a *different* detail issue between dispatch
//     and arrival. dm.applyMutation would silently drop the message
//     on the gen mismatch and leave the list cache stale. Mark the
//     cache stale here so the next list refetch (or SSE invalidation)
//     repopulates the rows the original mutation touched.
//
// Without case (3), a "close issue A in detail → jump to issue B
// before the close completes" sequence would update neither A's UI
// (it's gone) nor any cache, and the list rows would stay stale
// until an unrelated SSE event happened to refresh them.
func (m Model) routeMutation(mut mutationDoneMsg) (tea.Model, tea.Cmd) {
	if mut.origin == "form" {
		return m.routeFormMutation(mut)
	}
	// listIsActive / detailIsActive abstract over stacked vs. split
	// layout so the origin↔active-pane match is correct in both
	// modes. In split layout both panes are visible, but only the
	// focused pane owns key dispatch — so a mutation whose origin
	// pane isn't the focused pane still applies directly (case 1/2).
	if mut.origin == "list" && !m.listIsActive() {
		var cmd tea.Cmd
		m.list, cmd = m.list.applyMutation(mut, m.api, m.scope)
		return m, withConnGen(cmd, m.connGen)
	}
	if mut.origin == "detail" {
		if !m.detailIsActive() {
			var cmd tea.Cmd
			m.detail, cmd = m.detail.applyMutation(mut, m.api)
			return m, withConnGen(cmd, m.connGen)
		}
		if mut.gen != m.detail.gen {
			// Stale-to-current-detail: the original UI is gone but
			// the underlying data still changed. Mark the list cache
			// stale and schedule a debounced refetch so the rows the
			// original mutation touched repopulate without waiting
			// for SSE (roborev #102 finding 1 follow-up).
			if m.cache != nil {
				m.cache.markStale()
			}
			if !m.pendingRefetch && m.api != nil {
				m.pendingRefetch = true
				return m, debouncedRefetch(refetchDebounce)
			}
			return m, nil
		}
	}
	next, cmd := m.routeMutationToActivePane(mut)
	// Plan-8: label / create mutations may have changed the project's
	// label aggregate. Refresh the cache so the next `+` open shows
	// fresh counts. Doesn't wait on SSE — the user just took the
	// action; the menu should reflect it immediately.
	if mutAffectsLabelCounts(mut) {
		next, cmd = batchLabelRefresh(next, cmd, mut)
	}
	return next, withConnGen(cmd, m.connGen)
}

// mutAffectsLabelCounts reports whether a successful mutation could
// have changed the project's label aggregate. Label add/remove and
// create (which since Plan 8 commit 4 may carry labels via the
// multi-field new-issue form) all qualify; the cache refresh is gated
// against the per-project entry's existence so a plain-title create
// against a project that never had a label menu opened remains a
// zero-cost no-op.
func mutAffectsLabelCounts(mut mutationDoneMsg) bool {
	if mut.err != nil {
		return false
	}
	switch mut.kind {
	case "label.add", "label.remove", "create":
		return true
	}
	return false
}

// batchLabelRefresh dispatches a project-label cache refresh for the
// project the mutation touched, batching the resulting cmd with any
// existing cmd from the per-view dispatch. Returns the new model and
// the combined cmd so the caller can return both atomically.
//
// Gated on cache-entry existence: a project the user never opened the
// suggestion menu for has no entry, so a refetch would be a wasted
// HTTP roundtrip — same gate as maybeRefetchLabels for SSE events.
// The first time the user hits `+` against the project, the menu's
// own dispatch primes the entry; later mutations then refresh it.
func batchLabelRefresh(
	next tea.Model, prior tea.Cmd, mut mutationDoneMsg,
) (tea.Model, tea.Cmd) {
	nm, ok := next.(Model)
	if !ok {
		return next, prior
	}
	pid := projectIDFromMutation(nm, mut)
	if pid == 0 {
		return next, prior
	}
	if nm.projectLabels == nil {
		return next, prior
	}
	if _, exists := nm.projectLabels.byProject[pid]; !exists {
		return next, prior
	}
	nm, refresh := nm.dispatchLabelFetch(pid)
	if refresh == nil {
		return nm, prior
	}
	if prior == nil {
		return nm, refresh
	}
	return nm, tea.Batch(prior, refresh)
}

// projectIDFromMutation picks the project id the mutation acted on.
// Detail-side mutations carry it via dm.scopePID; list-side via the
// response's Issue.ProjectID; falls back to the active scope's
// projectID for shapes that omit it.
//
// Uses detailIsActive() so the dm.scopePID fallback fires in M6 split
// layout when the user is on focusDetail (m.view is still viewList in
// split). Without the helper a label-cache refresh on a detail-pane
// mutation could miss its project id and key off the active scope.
func projectIDFromMutation(m Model, mut mutationDoneMsg) int64 {
	if mut.resp != nil && mut.resp.Issue != nil && mut.resp.Issue.ProjectID != 0 {
		return mut.resp.Issue.ProjectID
	}
	if m.detailIsActive() && m.detail.scopePID != 0 {
		return m.detail.scopePID
	}
	return m.scope.projectID
}

// routeTopLevel handles non-SSE messages that the parent Model owns:
// resize, global quit, view-switch, detail-open/pop, input shell
// open/key. ok=true means the message was handled here.
func (m Model) routeTopLevel(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.MouseMsg:
		next, cmd := m.routeMouse(msg)
		return next, cmd, true
	case tea.WindowSizeMsg:
		prevLayout := m.layout
		m.width, m.height = msg.Width, msg.Height
		m.layout = m.resolveLayout()
		var flipCmd tea.Cmd
		if prevLayout != m.layout {
			m, flipCmd = m.handleLayoutFlip(prevLayout)
		}
		// Cache the terminal/detail viewport so PgDn can clamp body
		// scroll against the same dimensions the renderer will use.
		m.detail = m.applyDetailViewportCache(m.detail)
		return m, flipCmd, true
	case tea.KeyMsg:
		// Modal owns input when active. Enter the modal-specific
		// handler before falling through to input/global routing.
		if m.modal != modalNone {
			next, cmd := m.routeModalKey(msg)
			return next, cmd, true
		}
		if m.input.kind != inputNone {
			next, cmd := m.routeInputKey(msg)
			return next, cmd, true
		}
		if next, cmd, ok := m.routeGlobalKey(msg); ok {
			return next, cmd, true
		}
		if m.view == viewProjects {
			next, cmd := m.routeProjectsViewKey(msg)
			return next, cmd, true
		}
		if m.view == viewDaemons {
			next, cmd := m.routeDaemonsViewKey(msg)
			return next, cmd, true
		}
		// Detail-view `e` and `c` open M4 centered forms instead of
		// shelling out to $EDITOR. Routed at the Model level because
		// the form lives on m.input, which detail.Update can't reach.
		if next, cmd, ok := m.routeDetailFormKey(msg); ok {
			return next, cmd, true
		}
	case openInputMsg:
		next, cmd := m.openInputFromMsg(msg)
		return next, cmd, true
	case openDetailMsg:
		if m.staleConnMsg(msg.connGen) {
			return m, nil, true
		}
		next, cmd := m.handleOpenDetail(msg)
		return next, cmd, true
	case jumpDetailMsg:
		if m.staleConnMsg(msg.connGen) {
			return m, nil, true
		}
		next, cmd := m.handleJumpDetail(msg)
		return next, cmd, true
	case popDetailMsg:
		if m.staleConnMsg(msg.connGen) {
			return m, nil, true
		}
		m.view = viewList
		m.focus = focusList
		return m, nil, true
	}
	return m, nil, false
}

// applyDetailViewportCache copies the latest terminal dimensions into
// dm and recomputes the split-pane cache. Run this after installing a
// fresh detailModel (open/jump/follow) or after a layout toggle so
// PgUp/PgDn page-stepping and scrollViewportBy's EOF clamp see correct
// viewport dimensions on the very first key press, without waiting
// for the next tea.WindowSizeMsg. Without this seed, viewportDims
// returns ok=false and pageStep degrades to detailFallbackPageStep.
func (m Model) applyDetailViewportCache(dm detailModel) detailModel {
	dm.lastTermWidth = m.width
	dm.lastTermHeight = m.height
	return m.cacheDetailViewport(dm)
}

func (m Model) cacheDetailViewport(dm detailModel) detailModel {
	dm.lastDetailWidth = 0
	dm.lastDetailHeight = 0
	dm.lastDetailSplit = false
	if m.layout != layoutSplit {
		return dm
	}
	footerLines := helpLines(m.splitHelpRows(), m.width)
	bodyHeight := m.height - 2 - footerLines
	if bodyHeight < 4 {
		bodyHeight = 4
	}
	detailW := m.width - splitListPaneWidth(m.width)
	if detailW < 20 {
		detailW = 20
	}
	innerW := detailW - 2
	innerH := bodyHeight - 2
	if innerW < 10 {
		innerW = 10
	}
	if innerH < 2 {
		innerH = 2
	}
	dm.lastDetailWidth = innerW
	dm.lastDetailHeight = innerH
	dm.lastDetailSplit = true
	return dm
}

// openInput constructs the inputState for a kind and applies the
// initial state mutations the input needs (e.g. preFilter snapshot
// for bars; issue context for panel prompts). For inline command
// bars, the search/owner buffer pre-fills from the existing filter
// so the user can refine an active filter without retyping. For
// panel-local prompts, the issue number lands in the prompt title
// AND the formTarget rides on inputState.target so the autocomplete
// dispatch (label suggestions) can scope to the right project. For
// the centered new-issue form (Plan 8 commit 4), the form has no
// issue context — ctrl+s dispatches CreateIssue with the four-field
// payload. For other centered forms (edit-body, comment), openInput
// delegates to openBodyEditForm / openCommentForm — they need extra
// context (current body, issue target) so they're called directly
// from the detail key handler instead of via openInputMsg.
//
// inputLabelPrompt opens with a parallel dispatchLabelFetch so the
// suggestion menu can render its first frame populated (or with a
// "loading…" placeholder for the round trip's duration). Other
// panel-prompt kinds don't need a fetch — the `-` prompt sources
// from dm.issue.Labels (already loaded), and the others (owner /
// parent / blocker / link) have no autocomplete in this commit.
func (m Model) openInput(kind inputKind) (Model, tea.Cmd) {
	return m.openInputFromMsg(openInputMsg{kind: kind})
}

func (m Model) openInputFromMsg(msg openInputMsg) (Model, tea.Cmd) {
	kind := msg.kind
	switch {
	case kind == inputSearchBar:
		m.input = newSearchBar(m.list.filter)
	case kind == inputNewIssueForm:
		m.nextFormGen++
		s := newNewIssueForm()
		if msg.parentShortID != nil {
			s = newNewIssueFormWithParent(*msg.parentShortID)
		}
		s.formGen = m.nextFormGen
		m.input = s
	case kind == inputFilterForm:
		m.nextFormGen++
		s := newFilterForm(m.list.filter)
		s.formGen = m.nextFormGen
		m.input = s
	case kind.isPanelPrompt():
		target := m.panelPromptTarget()
		m.input = newPanelPrompt(kind, target)
		if kind == inputLabelPrompt {
			return m.dispatchLabelFetchIfNeeded(target.projectID)
		}
	}
	return m, nil
}

// panelPromptTarget builds the formTarget for a detail-side panel
// prompt: scopePID is authoritative (works for both single-project
// and all-projects scope), issueShortID + detailGen come from the
// open detail's identity. Zero values when no detail is open
// (defensive — shouldn't happen via the normal key path).
func (m Model) panelPromptTarget() formTarget {
	if m.detail.issue == nil {
		return formTarget{}
	}
	return formTarget{
		projectID:    m.detail.scopePID,
		issueShortID: m.detail.issue.ShortID,
		detailGen:    m.detail.gen,
	}
}

// dispatchLabelFetchIfNeeded triggers a label fetch only when the
// cache has no entry for pid. Re-opening the prompt against a project
// we already cached must NOT churn a redundant request — the cache's
// existing entry (and any in-flight fetch flagged on it) is the
// authoritative source. Returns the model and either the dispatch
// cmd or nil.
func (m Model) dispatchLabelFetchIfNeeded(pid int64) (Model, tea.Cmd) {
	if pid == 0 || m.projectLabels == nil {
		return m, nil
	}
	if _, ok := m.projectLabels.byProject[pid]; ok {
		return m, nil
	}
	return m.dispatchLabelFetch(pid)
}

// openBodyEditForm opens the centered body editor for the currently-
// open detail issue. Allocates a fresh formGen so a stale editor
// return from a previous form is rejected. Returns the model
// untouched if there's no open detail issue.
//
// projectID comes from m.detail.scopePID (the issue's actual project),
// not m.scope.projectID — the latter is 0 in all-projects scope and
// would route the save to /api/v1/projects/0/...
func (m Model) openBodyEditForm() Model {
	if m.detail.issue == nil {
		return m
	}
	target := formTarget{
		projectID:    m.detail.scopePID,
		issueShortID: m.detail.issue.ShortID,
		detailGen:    m.detail.gen,
	}
	m.nextFormGen++
	form := newBodyEditForm(target, m.detail.issue.Body)
	form.formGen = m.nextFormGen
	m.input = form
	return m
}

// openCommentForm opens the centered comment editor for the
// currently-open detail issue. See openBodyEditForm for why projectID
// is sourced from m.detail.scopePID, not m.scope.projectID.
func (m Model) openCommentForm() Model {
	if m.detail.issue == nil {
		return m
	}
	target := formTarget{
		projectID:    m.detail.scopePID,
		issueShortID: m.detail.issue.ShortID,
		detailGen:    m.detail.gen,
	}
	m.nextFormGen++
	form := newCommentForm(target)
	form.formGen = m.nextFormGen
	m.input = form
	return m
}

// routeInputKey delivers a key into the active input shell and
// applies the resulting action. Bars apply their buffer to lm.filter
// live on every keystroke (no debounce — filters are client-side).
// Panel prompts (M3b) commit on action only — no live mirror; they
// dispatch the mutation via dispatchPanelPromptCommit. Commit closes
// the input; cancel restores any pre-open snapshot (bars only).
//
// Label prompts (`+` / `-`) post-process the input: ↑/↓ already
// adjusted suggestHighlight in inputState.Update; we wrap it modulo
// the visible-suggestion count here. Tab completes the buffer to the
// highlighted suggestion's label (suggestion source is computed at
// the Model level — see suggestionsForPrompt).
func (m Model) routeInputKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	prevKind := m.input.kind
	next, action := m.input.Update(msg)
	m.input = next
	switch action {
	case actionCommit:
		return m.commitInput()
	case actionCancel:
		return m.cancelInput()
	case actionEditorHandoff:
		return m.handoffToEditor()
	}
	if m.input.kind.isCommandBar() {
		m = m.applyLiveBarFilter()
	}
	if isLabelPromptKind(prevKind) {
		m = m.applyLabelPromptKey(msg)
	}
	return m, nil
}

// applyLabelPromptKey post-processes a key into a label prompt:
// wraps suggestHighlight modulo the suggestion count and (on Tab)
// completes the buffer to the highlighted suggestion's label. The
// input layer signals "handled" for ↑/↓/⇥ but doesn't have the
// suggestion list in scope; this is where the wrap + completion run.
func (m Model) applyLabelPromptKey(msg tea.KeyMsg) Model {
	all := m.suggestionsForPrompt(m.input)
	buf := ""
	if f := m.input.activeField(); f != nil {
		buf = f.value()
	}
	visible := filterSuggestions(all, buf)
	n := len(visible)
	if n > 0 {
		m.input.suggestHighlight = ((m.input.suggestHighlight % n) + n) % n
	} else {
		m.input.suggestHighlight = 0
	}
	if msg.Type == tea.KeyTab && n > 0 {
		picked := visible[m.input.suggestHighlight].Label
		if f := m.input.activeField(); f != nil {
			f.setValue(picked)
			// SetCursor takes a rune index (clamped against the
			// bubbles textinput's internal []rune buffer); use
			// utf8.RuneCountInString so multi-byte labels position
			// the cursor at the end visually rather than past it.
			f.input.SetCursor(utf8.RuneCountInString(picked))
			m.input.fields[m.input.active] = *f
		}
	}
	return m
}

// suggestionsForPrompt returns the suggestion source for an open
// label prompt. `+` reads from the per-project label cache; `-` from
// the open detail's currently-attached labels (no separate fetch
// needed — dm.issue.Labels is authoritative). Returns nil for any
// other kind so callers can use the empty slice as a "no menu" gate.
func (m Model) suggestionsForPrompt(s inputState) []LabelCount {
	switch s.kind {
	case inputLabelPrompt:
		if m.projectLabels == nil {
			return nil
		}
		return m.projectLabels.byProject[s.target.projectID].labels
	case inputRemoveLabelPrompt:
		var attached []string
		if m.detail.issue != nil {
			attached = m.detail.issue.Labels
		}
		out := make([]LabelCount, len(attached))
		for i, l := range attached {
			// Count is irrelevant for `-` — the renderer omits the
			// count column when every entry has count==0.
			out[i] = LabelCount{Label: l, Count: 0}
		}
		return out
	}
	return nil
}

// filterSuggestions returns a copy of all sorted by count desc, then
// label asc, with a case-insensitive prefix filter applied. For `-`
// where counts are 0, the count tie reduces the order to label asc
// — the secondary sort is the effective one.
func filterSuggestions(all []LabelCount, prefix string) []LabelCount {
	out := make([]LabelCount, 0, len(all))
	pfx := strings.ToLower(strings.TrimSpace(prefix))
	for _, lc := range all {
		if pfx == "" || strings.HasPrefix(strings.ToLower(lc.Label), pfx) {
			out = append(out, lc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// handoffToEditor launches editorCmd on the current form's buffer,
// tagging the request with the form's formGen so the eventual
// editorReturnedMsg can reject itself if the form was closed or
// re-opened in the meantime. The form state stays in m.input — the
// editor return writes back into the textarea instead of submitting.
func (m Model) handoffToEditor() (Model, tea.Cmd) {
	if !m.input.kind.isCenteredForm() {
		return m, nil
	}
	f := m.input.activeField()
	if f == nil {
		return m, nil
	}
	editorKind := editorKindFor(m.input.kind)
	return m, editorCmd(editorKind, f.value(), m.input.formGen)
}

// routeDetailFormKey intercepts the detail-view `e` and `c` keys
// and opens the corresponding centered form instead of letting them
// reach the (now-removed) shell-out path. Returns ok=false for
// non-detail views so the key falls through to dispatchToView.
//
// The form needs Model-level state (m.input + nextFormGen counter),
// so this can't live in detail.Update. Gated by view + the absence
// of an open issue so an `e` press during loading is a no-op.
//
// In M6 split layout, the same gates apply but use focus instead of
// view: the detail pane only owns `e` / `c` when m.focus ==
// focusDetail. This keeps the list-pane `c` (clear filters) reachable
// when focus is on the list.
func (m Model) routeDetailFormKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	if !m.detailIsActive() || m.detail.issue == nil {
		return m, nil, false
	}
	switch {
	case m.keymap.EditBody.matches(msg):
		return m.openBodyEditForm(), nil, true
	case m.keymap.NewComment.matches(msg):
		return m.openCommentForm(), nil, true
	}
	return m, nil, false
}

// detailIsActive reports whether the detail pane currently owns key
// dispatch. In stacked layout that's m.view == viewDetail; in split
// layout it's m.focus == focusDetail. Used by routeDetailFormKey
// and other Model-level handlers that need to act on "the user is
// looking at detail right now."
func (m Model) detailIsActive() bool {
	if m.layout == layoutSplit {
		return m.focus == focusDetail
	}
	return m.view == viewDetail
}

// detailPaneVisible reports whether the detail pane is currently
// visible to the user. True for: viewDetail in stacked, OR viewList
// in split mode (where the detail pane sits next to the list). False
// for viewHelp / viewEmpty (full-screen overlays that hide both
// panes) or when the layout doesn't show detail at all.
//
// detailIsActive() alone is insufficient: in split mode focus can be
// focusDetail while m.view == viewHelp or viewEmpty (a full-screen
// overlay that hides the detail pane). Async messages — most notably
// jumpDetailMsg — must reject those hidden-view cases so a queued
// jump can't silently mutate the hidden detail state and dispatch
// fetches against an issue the user is no longer looking at.
func (m Model) detailPaneVisible() bool {
	if m.view != viewList && m.view != viewDetail {
		return false
	}
	return m.detailIsActive()
}

// listIsActive is the focusList counterpart of detailIsActive. In
// stacked layout it's m.view == viewList; in split layout it's
// m.focus == focusList.
func (m Model) listIsActive() bool {
	if m.layout == layoutSplit {
		return m.focus == focusList
	}
	return m.view == viewList
}

// routeMutationToActivePane is the active-pane mutation dispatcher.
// In stacked layout it forwards to the m.view-keyed pane (matching
// the existing dispatchToView path); in split layout it forwards to
// the focused pane regardless of m.view.
func (m Model) routeMutationToActivePane(mut mutationDoneMsg) (tea.Model, tea.Cmd) {
	if m.layout == layoutSplit {
		if mut.origin == "list" {
			var cmd tea.Cmd
			m.list, cmd = m.list.applyMutation(mut, m.api, m.scope)
			return m, cmd
		}
		var cmd tea.Cmd
		m.detail, cmd = m.detail.applyMutation(mut, m.api)
		return m, cmd
	}
	return m.dispatchToView(mut)
}

// editorKindFor maps a form kind onto the editorReturnedMsg kind tag.
// The tag is informational only at the Model layer (the formGen is
// what gates routing) but kept consistent with editor.go for future
// reuse. The new-issue form returns "create" so a future log/audit
// path can attribute the editor session correctly.
func editorKindFor(k inputKind) string {
	switch k {
	case inputCommentForm:
		return "comment"
	case inputBodyEditForm:
		return "edit"
	case inputNewIssueForm:
		return "create"
	}
	return ""
}

// routeFormMutation dispatches a form-originated mutationDoneMsg.
// Success: close the form and let the rest of the app re-fetch what
// it needs. Failure: surface the error on the form's status line and
// clear saving so the user can retry. A response that arrives after
// the user already cancelled the form (or it was somehow cleared) is
// dropped.
//
// Two success paths:
//
//  1. inputNewIssueForm — route through list create handling so
//     selectedUID seeds with the new issue's UID, the cursor
//     lands on it after refetch, and the list status hint surfaces
//     ("created #N"). The form does NOT auto-open the new issue's
//     detail view; the user lands back on the list with the new row
//     selected (Plan 8 commit 4 — replaces the M4 post-create chain).
//
//  2. inputBodyEditForm / inputCommentForm — re-classify as origin=
//     detail so the existing detail applyMutation logic refreshes
//     the body / comments list.
//
// formGen guard (jobs 242/244): a response whose formGen does not
// match m.input.formGen is dropped before any branching. This catches
// the stale-handoff race where the user submits form A, esc's, opens
// form B before A's response returns; without the guard the stale
// response could close form B (isCenteredForm() match), misroute the
// payload as origin=detail, or fire batchLabelRefresh against form
// A's project. Filter form is in isCenteredForm() too — the guard
// keeps it open whenever a stray non-filter form mutation arrives.
func (m Model) routeFormMutation(mut mutationDoneMsg) (tea.Model, tea.Cmd) {
	if !m.input.kind.isCenteredForm() {
		return m, nil
	}
	if mut.formGen != m.input.formGen {
		return m, nil
	}
	if mut.err != nil {
		m.input.saving = false
		m.input.err = mut.kind + " failed: " + mut.err.Error()
		return m, nil
	}
	if m.input.kind == inputNewIssueForm {
		m.input = inputState{}
		var cmd tea.Cmd
		m.list, cmd = m.list.applyMutation(mutationDoneMsg{
			origin: "list", kind: "create", resp: mut.resp,
		}, m.api, m.scope)
		// Form-create may carry labels (inline Labels field). The
		// daemon emits only issue.created (with labels folded into the
		// payload), NOT a separate issue.labeled event — so the
		// SSE-side maybeRefetchLabels will not fire. Without an
		// explicit refresh here, the per-project label aggregate would
		// stay stale until the next project switch / restart /
		// unrelated label SSE event. Use the same hook routeMutation
		// uses; the cache-existence gate inside batchLabelRefresh
		// keeps it a no-op for projects the user never opened the
		// menu for.
		if mutAffectsLabelCounts(mut) {
			next, cmd := batchLabelRefresh(m, cmd, mut)
			return next, withConnGen(cmd, m.connGen)
		}
		return m, withConnGen(cmd, m.connGen)
	}
	m.input = inputState{}
	// Hand off to the existing per-view mutation routing so the
	// detail's body / comments list updates. Re-classify as if it
	// came from detail (gen=current detail gen) so existing
	// applyMutation logic kicks in.
	mut.origin = "detail"
	mut.gen = m.detail.gen
	return m.routeMutation(mut)
}

// routeEditorReturn handles editorReturnedMsg at the Model level.
// formGen > 0 means the request came from a centered form's ctrl+e
// handoff; the return is matched against the currently-open form's
// formGen and either writes the content back into the textarea or
// is dropped as stale. formGen == 0 is the legacy detail-side
// shell-out path and falls through to dm.applyEditorReturned.
//
// On editor cancel/error (non-nil err), the form stays open with
// its previous buffer intact and the err surfaces on the form's
// status line. The textarea is NOT repopulated — preserves what the
// user typed before the editor opened.
func (m Model) routeEditorReturn(msg editorReturnedMsg) (Model, tea.Cmd) {
	if msg.formGen == 0 {
		return m, nil
	}
	if !m.input.kind.isCenteredForm() || m.input.formGen != msg.formGen {
		return m, nil
	}
	if msg.err != nil {
		m.input.err = "editor: " + msg.err.Error()
		return m, nil
	}
	if f := m.input.activeField(); f != nil {
		f.setValue(msg.content)
		m.input.fields[m.input.active] = *f
	}
	m.input.err = ""
	return m, nil
}

// applyLiveBarFilter mirrors the active bar's buffer into the
// corresponding lm.filter slot. Each keystroke re-applies the
// filter, which then narrows filteredIssues at render time without a
// network call (Search is client-side). Plan 8 commit 5a retired
// the owner bar; the filter modal's Owner axis takes its place and
// commits via commitFilterForm rather than per-keystroke.
func (m Model) applyLiveBarFilter() Model {
	if m.input.kind == inputNone {
		return m
	}
	field := m.input.activeField()
	if field == nil {
		return m
	}
	v := field.value()
	if m.input.kind == inputSearchBar {
		m.list.filter.Search = v
	}
	// Filter changed — clamp the cursor to the new visible-row count
	// so the highlighted row never falls past the end.
	m.list = m.list.clampCursorToFilter()
	return m
}

// commitInput closes the input shell. For command bars, the live-
// mirrored filter stays applied. For panel-local prompts, the
// trimmed buffer dispatches the corresponding detail-side mutation
// via dispatchPanelPromptCommit before the input clears.
//
// For centered forms, commitInput keeps the form open with
// saving=true while the mutation is in flight (so a duplicate
// ctrl+s is absorbed by the form's updateForm gate). The form is
// closed by routeFormMutation when the response arrives. Per-form
// gates: comments require non-empty content; body edits allow empty
// (clearing a body is legitimate); the new-issue form requires a
// non-blank Title and normalizes Labels/Owner.
//
// Plan 8 commit 5a: the filter form branches BEFORE the
// isCenteredForm() check. The filter form IS in isCenteredForm() for
// rendering (overlayModal needs to know to draw it), but its commit
// is filter-apply-and-refetch, not mutation-dispatch. Without the
// explicit early branch, ctrl+s on the filter modal would fall into
// commitFormInput, set saving=true, and wait forever for a
// mutationDoneMsg that never arrives (no daemon round-trip is in
// flight). Order matters here.
func (m Model) commitInput() (Model, tea.Cmd) {
	kind := m.input.kind
	if kind == inputFilterForm {
		return m.commitFilterForm(m.input)
	}
	rawBuf := ""
	if f := m.input.activeField(); f != nil {
		rawBuf = f.value()
	}
	if kind.isCenteredForm() {
		return m.commitFormInput(kind)
	}
	trimmed := strings.TrimSpace(rawBuf)
	m.input = inputState{}
	if kind.isPanelPrompt() && trimmed != "" {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.dispatchPanelPromptCommit(m.api, kind, trimmed)
		return m, cmd
	}
	return m, nil
}

// commitFilterForm reads the three filter axes off the form and
// applies them to lm.filter as a single atomic update. Cursor and
// selectedUID are reset so the next render lands on a fresh row
// (the prior selection may no longer match the new filter, and a
// clamp would be less predictable than starting at the top); the
// status line clears so any prior mutation hint doesn't linger over
// the new view. The form clears in one step; all filters are applied
// client-side over the cached all-status working set.
//
// Mirrors the s/c convention in applyFilterKey — explicit "I changed
// the filter" intent overrides the implicit "follow the same issue
// across refetches" intent.
func (m Model) commitFilterForm(form inputState) (Model, tea.Cmd) {
	if len(form.fields) < 4 {
		return m, nil
	}
	m.list.filter = ListFilter{
		Status: form.fields[0].radio.value(),
		Owner:  strings.TrimSpace(form.fields[1].input.Value()),
		Search: strings.TrimSpace(form.fields[2].input.Value()),
		Labels: normalizeLabels(form.fields[3].input.Value()),
	}
	// "all" is the surface label for "no Status filter"; the wire
	// expects an empty string.
	if m.list.filter.Status == "all" {
		m.list.filter.Status = ""
	}
	m.list.cursor = 0
	m.list.selectedUID = ""
	m.list.status = ""
	m.input = inputState{}
	return m, nil
}

// commitFormInput handles ctrl+s on a centered form. The form stays
// open with saving=true while the daemon round-trip runs; the
// arriving mutationDoneMsg closes it (success: clear and update
// detail / list; error: surface on the form's status line and leave
// open). Render-side sanitization elsewhere handles display safety;
// mutation payloads go to the wire untouched so the user's content
// is preserved exactly.
//
// formGen is captured before dispatch and threaded into the response
// so routeFormMutation can drop a stale response that arrives after
// the user closed this form and opened a different one (jobs 242/244).
func (m Model) commitFormInput(kind inputKind) (Model, tea.Cmd) {
	if kind == inputNewIssueForm {
		return m.commitNewIssueForm()
	}
	rawBuf := ""
	if f := m.input.activeField(); f != nil {
		rawBuf = f.value()
	}
	if kind == inputCommentForm && strings.TrimSpace(rawBuf) == "" {
		m.input.err = "comment cannot be empty"
		return m, nil
	}
	m.input.saving = true
	m.input.err = ""
	target := m.input.target
	formGen := m.input.formGen
	switch kind {
	case inputCommentForm:
		return m, withConnGen(dispatchFormAddComment(m.api, target, rawBuf, m.list.actor, formGen), m.connGen)
	case inputBodyEditForm:
		return m, withConnGen(dispatchFormEditBody(m.api, target, rawBuf, m.list.actor, formGen), m.connGen)
	}
	return m, nil
}

// commitNewIssueForm reads the five fields, normalizes Labels, Owner,
// and Parent, gates on a non-blank Title, and dispatches CreateIssue.
// Title is sent untrimmed so deliberate leading/trailing whitespace
// survives (mirrors the legacy inline-row contract). Labels are
// comma-split with per-token TrimSpace; empty tokens drop. Owner is
// nil-on-wire when blank after trim, otherwise the trimmed value.
//
// formGen is captured before dispatch and rides on the response so
// routeFormMutation can drop a stale response that lands after the
// user closed this form and opened another (jobs 242/244).
//
// projectID resolution: when detail is active (stacked viewDetail OR
// split-layout viewList with the detail pane focused) we use
// m.detail.scopePID — the open issue's actual project — so a
// child-create from a detail opened in all-projects scope still posts
// to the right project. Otherwise we use m.scope.projectID (the list
// view's "n" handler is gated against all-projects scope, so this is
// non-zero in that path).
func (m Model) commitNewIssueForm() (Model, tea.Cmd) {
	if len(m.input.fields) < 5 {
		return m, nil
	}
	title := m.input.fields[0].input.Value()
	if strings.TrimSpace(title) == "" {
		m.input.err = "title is required"
		return m, nil
	}
	body, err := newIssueBodyFromForm(m.input.fields, m.list.actor)
	if err != nil {
		m.input.err = err.Error()
		return m, nil
	}
	m.input.saving = true
	m.input.err = ""
	// detailIsActive() covers both stacked (m.view == viewDetail) and
	// split (m.view == viewList with the detail pane focused) layouts;
	// without it, "N" from a split-layout detail in all-projects scope
	// would dispatch a create against m.scope.projectID == 0.
	pid := m.scope.projectID
	if m.detailIsActive() && m.detail.scopePID != 0 {
		pid = m.detail.scopePID
	}
	return m, withConnGen(dispatchFormCreateIssue(m.api, pid, body, m.input.formGen), m.connGen)
}

func newIssueBodyFromForm(fields []inputField, actor string) (CreateIssueBody, error) {
	if len(fields) < 5 {
		return CreateIssueBody{}, fmt.Errorf("new issue form is incomplete")
	}
	parent, err := normalizeParentRef(fields[newIssueFormParentIndex].input.Value())
	if err != nil {
		return CreateIssueBody{}, err
	}
	body := CreateIssueBody{
		Title:  fields[0].input.Value(),
		Body:   fields[1].area.Value(),
		Labels: normalizeLabels(fields[2].input.Value()),
		Owner:  normalizeOwner(fields[3].input.Value()),
		Actor:  actor,
	}
	if parent != "" {
		body.Links = []CreateInitialLinkBody{{
			Type:  "parent",
			ToRef: parent,
		}}
	}
	return body, nil
}

// normalizeParentRef trims a parent-field buffer, strips a leading "#"
// sigil, and returns the bare ref. Empty input means "no parent".
// Validation of the ref shape (short_id length, ULID format) belongs to
// the daemon — surfacing parse errors here would require duplicating
// that policy, and the daemon's 400 is the same UX after one round
// trip.
func normalizeParentRef(buf string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(buf), "#")
	return trimmed, nil
}

// normalizeLabels splits buf on commas, trims whitespace per token,
// and drops empty tokens. Returns nil for an all-empty buffer so the
// JSON omitempty tag elides Labels from the wire.
func normalizeLabels(buf string) []string {
	parts := strings.Split(buf, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeOwner trims buf and returns nil when blank, else &trimmed.
// nil omits Owner from the wire (the daemon treats omission as "no
// owner change") rather than sending an empty string that would set
// the owner to the literal "".
func normalizeOwner(buf string) *string {
	t := strings.TrimSpace(buf)
	if t == "" {
		return nil
	}
	return &t
}

// dispatchFormCreateIssue is the form-side CreateIssue dispatch.
// Tagged with origin="form" + the captured formGen so
// routeFormMutation can match the response to the still-open form
// (and drop it cleanly if the user opened a different form first).
// Title is sent untrimmed; Labels and Owner have already been
// normalized by commitNewIssueForm.
func dispatchFormCreateIssue(
	api listAPI, pid int64, body CreateIssueBody, formGen int64,
) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.CreateIssue(ctx, pid, body)
		return mutationDoneMsg{
			origin: "form", kind: "create", formGen: formGen,
			resp: resp, err: err,
		}
	}
}

// dispatchFormAddComment is the form-side AddComment dispatch. Tagged
// with origin="form" + the captured formGen so routeFormMutation can
// match the response to the still-open form.
func dispatchFormAddComment(
	api detailAPI, target formTarget, body, actor string, formGen int64,
) tea.Cmd {
	pid, ref := target.projectID, target.issueShortID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.AddComment(ctx, pid, ref, body, actor)
		return mutationDoneMsg{
			origin: "form", kind: "form.comment.add", formGen: formGen,
			resp: resp, err: err,
		}
	}
}

// dispatchFormEditBody is the form-side EditBody dispatch. Same
// shape as dispatchFormAddComment.
func dispatchFormEditBody(
	api detailAPI, target formTarget, body, actor string, formGen int64,
) tea.Cmd {
	pid, ref := target.projectID, target.issueShortID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := api.EditBody(ctx, pid, ref, body, actor)
		return mutationDoneMsg{
			origin: "form", kind: "form.body.edit", formGen: formGen,
			resp: resp, err: err,
		}
	}
}

// cancelInput restores any pre-open filter snapshot (bars + filter
// modal) and closes the input — undoing every live keystroke the
// user typed. Panel-local prompts and centered forms (other than the
// filter modal) have no live mirror, so cancel is just close.
//
// The filter form's commit ALSO writes lm.filter, so a cancel that
// arrives BEFORE the commit only needs to clear the open form (no
// snapshot restore is needed because the filter never moved). The
// preFilter restore here is a no-op in that case but kept symmetric
// with the bar path so any future "live preview" mode plugs in
// without changing this code.
func (m Model) cancelInput() (Model, tea.Cmd) {
	if m.input.kind.isCommandBar() || m.input.kind == inputFilterForm {
		m.list.filter = m.input.preFilter
		m.list = m.list.clampCursorToFilter()
	}
	m.input = inputState{}
	return m, nil
}

// routeGlobalKey handles the global key family (quit, help, scope
// toggle), gated by canQuit so an open input/modal absorbs the key.
// viewEmpty honors quit/ctrl+c and the daemon picker so a user can
// recover from an empty daemon by switching to another configured one.
//
// `q` opens the quit-confirm modal (msgvault pattern); `ctrl+c`
// remains the immediate-quit escape hatch for power users.
//
// M6: layout-aware focus moves (tab/enter from focusList →
// focusDetail; esc from focusDetail → focusList) are checked AFTER
// the global keys so `q` still opens the quit confirm and `?` still
// toggles help in either focus state.
func (m Model) routeGlobalKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	if !m.canQuit() {
		return m, nil, false
	}
	// ctrl+c bypasses the confirm modal — fast quit for power users.
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit, true
	}
	if m.keymap.Quit.matches(msg) {
		m.modal = modalQuitConfirm
		return m, nil, true
	}
	if m.keymap.Daemons.matches(msg) {
		next, cmd := m.transitionToDaemons()
		return next, cmd, true
	}
	if m.view == viewEmpty {
		return m, nil, true
	}
	if m.keymap.Help.matches(msg) {
		return m.toggleHelp(), nil, true
	}
	if m.keymap.Projects.matches(msg) {
		next, cmd := m.transitionToProjects()
		return next, cmd, true
	}
	if m.keymap.ToggleLayout.matches(msg) {
		next, cmd := m.toggleLayout()
		return next, cmd, true
	}
	if next, cmd, ok := m.routeLayoutFocusKey(msg); ok {
		return next, cmd, true
	}
	return m, nil, false
}

// routeLayoutFocusKey handles M6 split-mode focus moves: tab from
// focusList → focusDetail; esc from focusDetail → focusList.
// Returns ok=true when the key was consumed by the focus layer so
// the caller doesn't fall through to per-view dispatch (which would
// also consume tab/esc).
//
// Enter on focusList is NOT consumed here so the list handler can
// still dispatch openDetailMsg (which seeds dm.issue if needed).
// The handleOpenDetail handler moves focus to focusDetail after
// seeding the detail; that keeps the list handler's
// openDetailMsg-dispatch flow intact while still honoring the spec
// "enter from list → detail" — see handleOpenDetail.
//
// In stacked layout the function is a no-op (the layout has only
// one pane; tab/esc retain their per-view meanings).
func (m Model) routeLayoutFocusKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	if m.layout != layoutSplit {
		return m, nil, false
	}
	if m.focus == focusList && msg.Type == tea.KeyTab && m.detail.issue != nil {
		m.focus = focusDetail
		return m, nil, true
	}
	if m.focus == focusDetail && msg.Type == tea.KeyEsc {
		if len(m.detail.navStack) > 0 {
			return m, nil, false
		}
		m.focus = focusList
		m.view = viewList
		return m, nil, true
	}
	return m, nil, false
}

// toggleHelp swaps between viewHelp and the previous view. Pressing ?
// from list/detail enters viewHelp; pressing ? from viewHelp restores
// whatever view the user came from. prevView is preserved so the round
// trip is reversible — q from viewHelp still quits per routeGlobalKey.
func (m Model) toggleHelp() Model {
	if m.view == viewHelp {
		m.view = m.prevView
		return m
	}
	m.prevView = m.view
	m.view = viewHelp
	return m
}

// routeSSE handles the SSE-side message family. Splitting this off
// Update keeps both functions inside the project's ≤8 cyclomatic
// budget. ok=true means the message was handled here.
//
// detailFollowTickMsg is the M6 split-mode cursor-debounce tick;
// it isn't an SSE message but it shares the same "tick fires →
// dispatch" shape and lives here for routing parity.
func (m Model) routeSSE(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case eventReceivedMsg:
		if m.staleConnMsg(msg.gen) {
			return m, m.waitForSSE(), true
		}
		next, cmd := m.handleEventReceived(msg)
		return next, cmd, true
	case resetRequiredMsg:
		if m.staleConnMsg(msg.gen) {
			return m, m.waitForSSE(), true
		}
		next, cmd := m.handleResetRequired(msg)
		return next, cmd, true
	case sseStatusMsg:
		if m.staleConnMsg(msg.gen) {
			return m, m.waitForSSE(), true
		}
		m.sseStatus = msg.state
		return m, m.waitForSSE(), true
	case refetchTickMsg:
		next, cmd := m.handleRefetchTick()
		return next, cmd, true
	case toastExpiredMsg:
		next, cmd := m.handleToastExpired()
		return next, cmd, true
	case detailFollowTickMsg:
		next, cmd := m.handleDetailFollowTick(msg)
		return next, cmd, true
	}
	return m, nil, false
}

func (m Model) staleConnMsg(gen uint64) bool {
	return gen != 0 && gen != m.connGen
}

func withConnGen(cmd tea.Cmd, gen uint64) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		return stampConnGen(cmd(), gen)
	}
}

func stampConnGen(msg tea.Msg, gen uint64) tea.Msg {
	switch msg := msg.(type) {
	case tea.BatchMsg:
		out := make(tea.BatchMsg, len(msg))
		for i, cmd := range msg {
			out[i] = withConnGen(cmd, gen)
		}
		return out
	case initialFetchMsg:
		msg.connGen = gen
		return msg
	case refetchedMsg:
		msg.connGen = gen
		return msg
	case mutationDoneMsg:
		msg.connGen = gen
		return msg
	case labelsFetchedMsg:
		msg.connGen = gen
		return msg
	case projectsLoadedMsg:
		msg.connGen = gen
		return msg
	case openDetailMsg:
		msg.connGen = gen
		return msg
	case jumpDetailMsg:
		msg.connGen = gen
		return msg
	case popDetailMsg:
		msg.connGen = gen
		return msg
	default:
		return msg
	}
}

func (m Model) staleConnResult(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case initialFetchMsg:
		return m.staleConnMsg(msg.connGen)
	case refetchedMsg:
		return m.staleConnMsg(msg.connGen)
	}
	return false
}

// populateCache updates the single-slot cache after a successful list
// fetch and forwards the result into lm.applyFetched so list state stays
// in sync with the cache. Doing this here (rather than in
// listModel.Update via dispatchToView) keeps the list rows fresh even
// when the help overlay or detail view is active when the fetch lands —
// otherwise toggling back to the list would render the pre-fetch
// snapshot. Errors still update lm.err and clear loading via
// applyFetched but leave the cache untouched so a transient failure
// does not erase the prior snapshot.
//
// Caller responsibility: drop stale fetches via isStaleListFetch
// before invoking populateCache — see Update.
func (m Model) populateCache(msg tea.Msg) Model {
	_, issues, err := fetchPayload(msg)
	if err == nil && m.cache != nil {
		issues, _ = trimQueueWorkingSet(issues)
		m.cache.put(m.currentCacheKey(), issues)
	}
	m.list = m.list.applyFetched(msg)
	return m
}

// currentCacheKey is the cacheKey for the current queue working set — the
// authority for "is this fetch still relevant" comparisons.
func (m Model) currentCacheKey() cacheKey {
	return cacheKey{
		allProjects: m.scope.allProjects,
		projectID:   m.scope.projectID,
		limit:       queueFetchLimit,
	}
}

// fetchPayload extracts (dispatchKey, issues, err) from the two list-
// fetch message shapes so populateCache can share one staleness +
// cache-update path across them.
func fetchPayload(msg tea.Msg) (cacheKey, []Issue, error) {
	switch m := msg.(type) {
	case initialFetchMsg:
		return m.dispatchKey, m.issues, m.err
	case refetchedMsg:
		return m.dispatchKey, m.issues, m.err
	}
	return cacheKey{}, nil, nil
}

// cacheKeysEqual reports whether two cacheKeys denote the same queue
// working set.
func cacheKeysEqual(a, b cacheKey) bool {
	return a.allProjects == b.allProjects &&
		a.projectID == b.projectID &&
		a.limit == b.limit
}

// handleEventReceived marks the cache stale, kicks off (or coalesces
// into) a 150ms-debounced refetch when the event affects the current
// view, refetches the open detail issue when the event names it, and
// always re-arms the SSE bridge so the next frame is awaited.
//
// Affects-view: in single-project scope an event is interesting only
// when it carries our projectID; in all-projects scope every event is
// interesting. Cross-project (projectID == 0) events fall through as
// "ignore" so an unscoped daemon push cannot churn an unrelated view.
//
// Label-cache invalidation runs alongside the list/detail refetch
// path: a label add/remove on the daemon side may have changed the
// project's label aggregate, so any cached entry for the event's
// project gets a fresh dispatch. This is a SECOND, additional cmd —
// it doesn't replace or short-circuit the list/detail refetch above.
func (m Model) handleEventReceived(msg eventReceivedMsg) (tea.Model, tea.Cmd) {
	cmds := []tea.Cmd{m.waitForSSE()}
	if m.eventAffectsView(msg) {
		m.cache.markStale()
		if !m.pendingRefetch {
			m.pendingRefetch = true
			cmds = append(cmds, debouncedRefetch(refetchDebounce))
		}
	}
	if m.view == viewProjects && eventAffectsProjectsTable(msg) {
		m.projectsStale = true
		m.projectsGen++
		if !m.projectsRefetchPending {
			m.projectsRefetchPending = true
			cmds = append(cmds, projectsDebounceCmd())
		}
	}
	if cmd := m.maybeRefetchOpenDetail(msg); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if next, cmd := m.maybeRefetchLabels(msg); cmd != nil {
		m = next
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// maybeRefetchLabels invalidates the project-label cache when an SSE
// label event names a project we have a cached entry for. The
// suggestion menu reads the cache on render, so a fresh dispatch
// keeps the `+` autocomplete in sync with the daemon's authoritative
// counts. No-op when the event isn't a label event, the cache has no
// entry for the project, or projectLabels was never initialized
// (defensive — the field is set by initialModel).
func (m Model) maybeRefetchLabels(msg eventReceivedMsg) (Model, tea.Cmd) {
	if msg.eventType != "issue.labeled" && msg.eventType != "issue.unlabeled" {
		return m, nil
	}
	if m.projectLabels == nil {
		return m, nil
	}
	if _, ok := m.projectLabels.byProject[msg.projectID]; !ok {
		return m, nil
	}
	return m.dispatchLabelFetch(msg.projectID)
}

// eventAffectsView is the per-message gate for invalidation. Returns
// true when the event's scope overlaps the current view's scope. An
// empty event projectID can be a system-wide event (not currently
// emitted) — we ignore it rather than refetch every time so a future
// daemon broadcast for an unscoped event can't churn the list.
func (m Model) eventAffectsView(msg eventReceivedMsg) bool {
	if msg.projectID == 0 {
		return false
	}
	if m.scope.allProjects {
		return true
	}
	return msg.projectID == m.scope.projectID
}

// maybeRefetchOpenDetail dispatches the four detail fetches (issue +
// per-tab) when an SSE event names the currently-open detail issue.
// All four run because the event-kind alone isn't enough to know which
// tab needs refreshing — for example, issue.commented refreshes
// comments but issue.linked refreshes links, and issue.relabeled
// touches the body header. Refetching all four is cheap (the daemon
// has these in cache) and keeps every tab fresh without a kind switch.
//
// The match requires both projectID and the watched issue's UID to
// align with the event's subject — UID is canonical and the
// authoritative key across short_id cutovers. Each fetch is tagged
// with the current detail-open gen so applyFetched drops the result
// if the user navigates away before the response lands.
func (m Model) maybeRefetchOpenDetail(msg eventReceivedMsg) tea.Cmd {
	if m.api == nil {
		return nil
	}
	// Detail is open if either the stacked view shows detail OR the
	// split layout has a detail issue loaded (regardless of focus —
	// the pane is visible either way and any SSE invalidation should
	// keep it fresh).
	if m.view != viewDetail && m.layout != layoutSplit {
		return nil
	}
	if m.detail.issue == nil {
		return nil
	}
	if msg.projectID != m.detail.scopePID {
		return nil
	}
	pid := m.detail.scopePID
	ref := m.detail.issue.ShortID
	uid := m.detail.issue.UID
	if !msg.matchesIssue(ref, uid) {
		return nil
	}
	gen := m.detail.gen
	return tea.Batch(
		fetchIssue(m.api, pid, ref, gen),
		fetchComments(m.api, pid, ref, gen),
		fetchEvents(m.api, pid, ref, gen),
		fetchLinks(m.api, pid, ref, gen),
	)
}

// matchesIssue reports whether the event names the watched issue.
// ref / uid describe the open detail pane; uid is the canonical key
// and wins whenever the event carries any peer UID. ref is the
// short_id snapshot used as a fallback only when no UIDs are present
// (currently unused in steady state — all daemon-emitted events post-
// kata#1 carry UIDs).
//
// UID-priority rule: if the event carries any UIDs for a particular
// match channel, the short_id fallback for THAT channel is suppressed.
// short_ids can collide across federation merges, so a UID mismatch
// must beat a short_id match — otherwise an open pane at UID-A would
// refresh on every event for UID-B that happens to share UID-A's
// short_id. See TestHandleEventReceived_LinksChangedUIDMismatchSameShortID.
func (msg eventReceivedMsg) matchesIssue(ref, uid string) bool {
	if msg.issueUID != "" && uid != "" && msg.issueUID == uid {
		return true
	}
	if msg.issueShortID != "" && msg.issueShortID == ref && msg.issueUID == "" {
		return true
	}
	if fromUID, toUID, ok := msg.parentLinkEndpointUIDs(); ok && uid != "" {
		if fromUID == uid || toUID == uid {
			return true
		}
	}
	if msg.linksChanged != nil && msg.linksChangedMatches(ref, uid) {
		return true
	}
	return false
}

// linksChangedMatches reports whether the event's linksChanged payload
// references the watched issue. UID-keyed match wins when the payload
// carries UIDs; the short_id-keyed fallback fires only when NO UIDs
// are present (legacy / synthetic events).
func (msg eventReceivedMsg) linksChangedMatches(ref, uid string) bool {
	lc := msg.linksChanged
	if uid != "" {
		if lc.SetUID == uid || lc.RemovedUID == uid {
			return true
		}
		for _, u := range lc.RefUIDs {
			if u == uid {
				return true
			}
		}
	}
	if lc.hasAnyUIDs() {
		return false
	}
	if ref == "" {
		return false
	}
	if lc.Set == ref || lc.Removed == ref {
		return true
	}
	for _, r := range lc.Refs {
		if r == ref {
			return true
		}
	}
	return false
}

// hasAnyUIDs reports whether the linksChanged payload carries any peer
// UIDs. When true, the short_id fallback is suppressed so a UID
// mismatch beats an incidental short_id collision.
func (lc *linksChangedParents) hasAnyUIDs() bool {
	if lc.SetUID != "" || lc.RemovedUID != "" {
		return true
	}
	return len(lc.RefUIDs) > 0
}

// parentLinkEndpointUIDs returns the (from_uid, to_uid) pair when the
// event is a parent-link signal: either a direct issue.linked /
// issue.unlinked frame whose link.Type == "parent", or an
// issue.created frame whose payload `links` array carries a parent
// entry (the agent-creates-subissue path). decodeEventReceived
// normalises the second case onto the same msg.link shape so this
// consumer doesn't have to branch on event type beyond the allowlist.
func (msg eventReceivedMsg) parentLinkEndpointUIDs() (fromUID, toUID string, ok bool) {
	switch msg.eventType {
	case "issue.linked", "issue.unlinked", "issue.created":
	default:
		return "", "", false
	}
	if msg.link == nil || msg.link.Type != "parent" {
		return "", "", false
	}
	if msg.link.FromIssueUID == "" || msg.link.ToIssueUID == "" {
		return "", "", false
	}
	return msg.link.FromIssueUID, msg.link.ToIssueUID, true
}

// handleRefetchTick fires after the debounce window. Clears the
// pending flag and dispatches a refetch when the cache is stale; if a
// race cleared the stale flag (e.g., a manual filter change refetched
// already), the tick is a no-op so we don't spin a redundant request.
func (m Model) handleRefetchTick() (tea.Model, tea.Cmd) {
	m.pendingRefetch = false
	if !m.cache.isStale() {
		return m, m.waitForSSE()
	}
	cmd := m.list.refetchCmd(m.api, m.scope)
	return m, tea.Batch(withConnGen(cmd, m.connGen), m.waitForSSE())
}

// handleResetRequired is the terminal-cache branch: drop everything,
// dispatch an immediate refetch, and surface a 2s 'resynced' toast so
// the user knows the view repopulated under their feet. We re-arm the
// SSE bridge so subsequent frames are awaited, but the goroutine that
// pushed this frame may itself have closed the stream — startSSE will
// reconnect from the same checkpoint via Last-Event-ID. The daemon's
// contract (internal/api/events.go EventReset.EventID == ResetAfterID)
// makes the SSE id: line on this frame the authoritative resume
// cursor, so resetRequiredMsg deliberately carries no payload.
func (m Model) handleResetRequired(_ resetRequiredMsg) (tea.Model, tea.Cmd) {
	m.cache.drop()
	m.pendingRefetch = false
	m.projectsStale = false
	m.projectsRefetchPending = false
	m.toast = &toast{
		text:      "resynced",
		level:     toastInfo,
		expiresAt: m.toastNow().Add(toastResyncedTTL),
	}
	cmds := []tea.Cmd{m.waitForSSE(), toastExpireCmd(toastResyncedTTL)}
	if m.api != nil {
		cmds = append(cmds, withConnGen(m.list.refetchCmd(m.api, m.scope), m.connGen))
		// If the user is in detail view, the open issue + tabs are also
		// stale — the cursor invalidation behind reset_required means
		// any cached detail data is suspect, not just the list. Batch
		// the four detail fetches so the active detail tab is fresh by
		// the next render.
		if cmd := m.refetchOpenDetail(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		// If the user is in viewProjects, the table numbers are also
		// stale — without this, the "resynced" toast lies because the
		// per-project counts wouldn't reflect the post-reset state.
		if m.view == viewProjects {
			cmds = append(cmds, m.fetchProjectsWithStats())
		}
	}
	return m, tea.Batch(cmds...)
}

// refetchOpenDetail batches the four detail fetches when the user is
// looking at a detail pane. Used by reset_required and any other path
// that needs to repopulate the open issue without an issue-targeted
// SSE event to drive maybeRefetchOpenDetail.
//
// In M6 split layout the detail pane is always visible when an
// issue is loaded (regardless of focus), so the same fetch fires
// to keep it fresh even while focus is on the list.
func (m Model) refetchOpenDetail() tea.Cmd {
	if m.api == nil || m.detail.issue == nil {
		return nil
	}
	if m.view != viewDetail && m.layout != layoutSplit {
		return nil
	}
	pid := m.detail.scopePID
	ref := m.detail.issue.ShortID
	gen := m.detail.gen
	return tea.Batch(
		fetchIssue(m.api, pid, ref, gen),
		fetchComments(m.api, pid, ref, gen),
		fetchEvents(m.api, pid, ref, gen),
		fetchLinks(m.api, pid, ref, gen),
	)
}

// handleToastExpired clears m.toast iff the active toast is past its
// expiry. The wall-clock check guards against a stale tick that fires
// after the user replaced the toast with a fresh one — we don't want
// the second toast to die on the first toast's timer.
func (m Model) handleToastExpired() (tea.Model, tea.Cmd) {
	if m.toast != nil && !m.toastNow().Before(m.toast.expiresAt) {
		m.toast = nil
	}
	return m, m.waitForSSE()
}

// refetchDebounce is the coalescing window for SSE-driven refetches.
// 150ms matches the master spec (§7.1) — long enough that a burst of
// related events (e.g., issue.created + issue.labeled within the same
// mutation) collapses to one fetch, short enough that the user sees
// fresh data before they take their next action.
const refetchDebounce = 150 * time.Millisecond

// toastResyncedTTL is how long the 'resynced' toast lingers before
// toastExpireCmd clears it. 2s matches the plan's spec.
const toastResyncedTTL = 2 * time.Second

// toastNoBindingTTL is how long the "no project bound" toast (R toggle
// without a default project) sticks around. Slightly longer than the
// resynced toast because the user has to act on the hint, not just notice.
const toastNoBindingTTL = 3 * time.Second

// debouncedRefetch returns a tea.Cmd that emits refetchTickMsg after d.
// The TEA loop receives the message, checks the cache, and dispatches
// the actual list refetch via lm.refetchCmd.
func debouncedRefetch(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return refetchTickMsg{} })
}

// toastExpireCmd schedules a toastExpiredMsg at d. The Update branch
// double-checks the wall clock before clearing the toast so a fresher
// toast cannot be cut short by an earlier timer.
func toastExpireCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return toastExpiredMsg{} })
}

// canQuit reports whether a global keystroke (q, ?, R) should be
// honored. False while an input shell (M3a/M3b/M3.5c bars/prompts
// /forms) or a confirm modal (M3.5b quit confirm) is open. Both
// gates redirect global keys into the field instead of firing.
func (m Model) canQuit() bool {
	if m.modal != modalNone {
		return false
	}
	if m.input.kind != inputNone {
		return false
	}
	return true
}

// routeModalKey delivers a key to the active centered modal. M3.5b
// only handles modalQuitConfirm: y/Y commits → tea.Quit; n/N/esc
// cancels → close the modal. Other keys are absorbed (the modal owns
// dispatch; nothing reaches the underlying view).
//
// ctrl+c always fast-quits regardless of which modal is open — the
// power-user escape hatch must not be trapped behind a confirmation
// (roborev #111 finding 1).
func (m Model) routeModalKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	switch m.modal {
	case modalQuitConfirm:
		switch msg.String() {
		case "y", "Y":
			return m, tea.Quit
		case "n", "N", "esc":
			m.modal = modalNone
			return m, nil
		}
	}
	return m, nil
}

// handleOpenDetail seeds m.detail with the chosen issue and dispatches
// the four concurrent fetches via tea.Batch. The fetches run in
// parallel so the user sees data on whichever tab is active first. The
// detail model also remembers the project_id and all-projects flag so
// the Enter-jump path has them without re-resolving scope.
//
// fetchIssue rides alongside the three tab fetches because the list-row
// Issue seeded above carries no Labels (list rows don't include them
// today) — without the show-response refresh, the detail header would
// stay label-less until a manual refetch. handleJumpDetail dispatches
// the same four-fetch batch; this brings the open-from-list path to
// parity. Same fetchIssue helper, additional call site.
//
// The detail-open generation is allocated from m.nextGen — a Model-
// level monotonic counter — so it never collides with a previously
// jumped-and-backed snapshot's gen. The actor is seeded from the list
// model so detail-side mutations carry the resolved identity rather
// than the empty string.
func (m Model) handleOpenDetail(msg openDetailMsg) (tea.Model, tea.Cmd) {
	iss := msg.issue
	pid := detailProjectID(iss, m.scope)
	m.nextGen++
	// Reset on open is the spec — no per-issue scroll memory.
	m.detail = newDetailModel()
	m.detail.gen = m.nextGen
	m.detail.issue = &iss
	m.detail.scopePID = pid
	m.detail.allProjects = m.scope.allProjects
	m.detail.actor = m.list.actor
	m.detail.uidFormat = m.uidFormat
	// Per-tab loading flags drive the placeholder strings until each
	// fetch returns; they're cleared (with the per-tab err set) by
	// applyFetched.
	m.detail.commentsLoading = true
	m.detail.eventsLoading = true
	m.detail.linksLoading = true
	m.detail = m.applyDetailViewportCache(m.detail)
	m.view = viewDetail
	// In M6 split layout, "enter from list → detail" means focus
	// moves to the detail pane on open. m.view also moves to
	// viewDetail above so a subsequent split→stacked layout flip
	// keeps the user on the detail pane.
	if m.layout == layoutSplit {
		m.focus = focusDetail
	}
	if m.api == nil {
		return m, nil
	}
	gen := m.detail.gen
	cmds := []tea.Cmd{
		fetchIssue(m.api, pid, iss.ShortID, gen),
		fetchComments(m.api, pid, iss.ShortID, gen),
		fetchEvents(m.api, pid, iss.ShortID, gen),
		fetchLinks(m.api, pid, iss.ShortID, gen),
	}
	return m, tea.Batch(cmds...)
}

// handleJumpDetail performs an Enter-jump from the detail view to a
// referenced issue. The current detailModel is snapshotted onto its
// own navStack so handleBack can restore it; a fresh detailModel is
// seeded with a new monotonic gen and the four fetches dispatch in
// parallel. The active tab and actor are preserved so the user lands
// in the same context.
//
// detail.handleEnter emits jumpDetailMsg rather than building the new
// detail itself: the gen must come from m.nextGen so a snapshot
// restored by handleBack with an older gen can't trick the next
// jump's gen into colliding with a stale fetch.
func (m Model) handleJumpDetail(msg jumpDetailMsg) (tea.Model, tea.Cmd) {
	if m.api == nil {
		return m, nil
	}
	// jumpDetailCmd is asynchronous (emits jumpDetailMsg via tea.Cmd),
	// so the user can pop back to the list — or the help overlay can
	// open — between the keypress and Model.Update seeing the message.
	// Without this guard the queued jump would mutate hidden detail
	// state and dispatch four fetches against an issue the user is no
	// longer looking at. Active-pane check first; navStack cap second.
	//
	// Use detailPaneVisible() (not m.view==viewDetail or bare
	// detailIsActive()) so the gate is correct in M6 split layout AND
	// rejects full-screen overlays (viewHelp/viewEmpty). detailIsActive()
	// alone misses the latter: in split mode focus can stay focusDetail
	// while m.view == viewHelp, which would let a queued jump mutate
	// the hidden detail state.
	if !m.detailPaneVisible() {
		return m, nil
	}
	if len(m.detail.navStack) >= detailNavCap {
		return m, nil
	}
	prior := m.detail
	prior.navStack = nil
	pid := m.detail.scopePID
	m.nextGen++
	gen := m.nextGen
	// tabExplicit carries forward with activeTab so a late-arriving
	// fetch on the jumped-to issue cannot stomp the preserved tab via
	// autoSelectActivityTab (roborev #17155 finding 2). Carrying the
	// tab without marking it explicit silently undid the user's
	// context when the new issue's first non-empty tab differed.
	next := detailModel{
		loading:         true,
		gen:             gen,
		activeTab:       m.detail.activeTab,
		tabExplicit:     true,
		navStack:        append(m.detail.navStack, prior),
		scopePID:        pid,
		allProjects:     m.detail.allProjects,
		actor:           m.detail.actor,
		uidFormat:       m.detail.uidFormat,
		commentsLoading: true,
		eventsLoading:   true,
		linksLoading:    true,
	}
	m.detail = m.applyDetailViewportCache(next)
	cmds := []tea.Cmd{
		fetchIssue(m.api, pid, msg.ref, gen),
		fetchComments(m.api, pid, msg.ref, gen),
		fetchEvents(m.api, pid, msg.ref, gen),
		fetchLinks(m.api, pid, msg.ref, gen),
	}
	return m, tea.Batch(cmds...)
}

// dispatchToView forwards msg to the active sub-view's Update.
//
// M6 split layout: dispatch follows m.focus (focusList → list,
// focusDetail → detail) so the user's key reaches the pane they're
// looking at. The view-vs-focus split ALSO ensures detail-only
// fetched messages (commentsFetchedMsg etc.) reach dm.Update even
// when m.view is viewList — necessary for the detail-follows-cursor
// path because the list pane keeps focus while detail fetches land.
//
// In stacked layout the original m.view path is preserved.
func (m Model) dispatchToView(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.layout == layoutSplit {
		return m.dispatchToSplitPane(msg)
	}
	switch m.view {
	case viewList:
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg, m.keymap, m.api, m.scope)
		return m, withConnGen(cmd, m.connGen)
	case viewDetail:
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg, m.keymap, m.api)
		return m, withConnGen(cmd, m.connGen)
	}
	return m, nil
}

// dispatchToSplitPane is the M6 split-mode dispatcher. Detail-only
// fetched messages (issue/comments/events/links) ALWAYS reach
// dm.Update so a fetched payload lands even while the list pane
// owns focus. Other messages go to the focused pane. Keys land on
// the focused pane only — never both panes.
func (m Model) dispatchToSplitPane(msg tea.Msg) (tea.Model, tea.Cmd) {
	if isDetailFetchMsg(msg) {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg, m.keymap, m.api)
		return m, withConnGen(cmd, m.connGen)
	}
	if _, ok := msg.(tea.KeyMsg); ok && m.focus == focusList {
		next, cmd := m.dispatchListKey(msg)
		return next, cmd
	}
	if _, ok := msg.(tea.KeyMsg); ok && m.focus == focusDetail {
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg, m.keymap, m.api)
		return m, withConnGen(cmd, m.connGen)
	}
	// Non-key, non-detail-fetch messages route to the focused pane so
	// list mutations / list refetches land where the user is looking.
	if m.focus == focusList {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg, m.keymap, m.api, m.scope)
		return m, withConnGen(cmd, m.connGen)
	}
	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg, m.keymap, m.api)
	return m, withConnGen(cmd, m.connGen)
}

// dispatchListKey routes a key into the list pane and, when the
// resulting highlighted row changed, retargets the detail pane onto
// the new row + schedules a 75ms-debounced detail fetch. This is
// the detail-follows-cursor mechanism that makes the split layout
// feel responsive: the user moves up/down in the list and the
// detail repaints synchronously with the highlighted issue's
// metadata, then the debounced fetch fills in the four detail
// fetches.
//
// "Highlighted row changed" is the composite (project_id, number)
// identity returned by pickHighlightedIssue — comparing
// selectedUID alone would miss the case where two rows share a
// Number across projects (in all-projects mode, currently gated off
// but the code is forward-looking). roborev #251 finding 1.
func (m Model) dispatchListKey(msg tea.Msg) (Model, tea.Cmd) {
	prevPID, prevUID, prevHas := highlightedIdentity(m.list)
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg, m.keymap, m.api, m.scope)
	newPID, newUID, newHas := highlightedIdentity(m.list)
	if prevHas == newHas && prevPID == newPID && prevUID == newUID {
		return m, withConnGen(cmd, m.connGen)
	}
	// Highlighted row changed. Retarget detail immediately so the
	// pane never lags the list.
	m, followCmd := m.scheduleDetailFollow()
	if followCmd == nil {
		return m, withConnGen(cmd, m.connGen)
	}
	if cmd == nil {
		return m, followCmd
	}
	return m, tea.Batch(withConnGen(cmd, m.connGen), followCmd)
}

// highlightedIdentity returns the composite (project_id, uid) of the
// row currently under lm.cursor. has=false when the filtered list is
// empty. Used by dispatchListKey to detect cross-project row changes
// or short_id collisions that the project-scoped short_id alone
// would conflate.
func highlightedIdentity(lm listModel) (pid int64, uid string, has bool) {
	iss, ok := pickHighlightedIssue(lm)
	if !ok {
		return 0, "", false
	}
	return iss.ProjectID, iss.UID, true
}

// isDetailFetchMsg reports whether msg is one of the four detail
// per-tab fetched message kinds. Used by dispatchToSplitPane to
// route fetched payloads to dm.Update regardless of focus so the
// detail pane stays in sync as the user navigates the list.
func isDetailFetchMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case detailFetchedMsg, commentsFetchedMsg, eventsFetchedMsg, linksFetchedMsg:
		return true
	}
	return false
}

// scheduleDetailFollow retargets m.detail.issue at the currently-
// highlighted list row and schedules a 75ms-debounced
// detailFollowTickMsg that will dispatch the four detail fetches
// when it fires. Bumps m.nextDetailFollowGen so a previously-pending
// tick (whose gen is now stale) drops on arrival without firing a
// fetch.
//
// When m.api is nil (unit tests that drive the model directly), the
// debounce tick still fires but the fetch dispatch short-circuits;
// the synchronous retarget of dm.issue still runs so tests can
// observe the detail pane following the cursor without standing up
// an http server.
func (m Model) scheduleDetailFollow() (Model, tea.Cmd) {
	iss, ok := pickHighlightedIssue(m.list)
	if !ok {
		return m, nil
	}
	pid := detailProjectID(iss, m.scope)
	m.nextGen++
	gen := m.nextGen
	// Reset detail to a fresh shell pinned to the highlighted issue.
	// per-tab loading flags drive the placeholders until the fetched
	// payload arrives; pre-existing tab data from the previous row
	// drops cleanly because the new gen invalidates older fetches.
	m.detail = newDetailModel()
	m.detail.gen = gen
	issCopy := iss
	m.detail.issue = &issCopy
	m.detail.scopePID = pid
	m.detail.allProjects = m.scope.allProjects
	m.detail.actor = m.list.actor
	m.detail.uidFormat = m.uidFormat
	m.detail.commentsLoading = true
	m.detail.eventsLoading = true
	m.detail.linksLoading = true
	m.detail = m.applyDetailViewportCache(m.detail)
	m.nextDetailFollowGen++
	tickGen := m.nextDetailFollowGen
	return m, tea.Tick(detailFollowDebounce, func(time.Time) tea.Msg {
		return detailFollowTickMsg{gen: tickGen}
	})
}

// detailFollowDebounce is the M6 split-mode cursor-debounce window.
// 75ms is short enough to feel responsive (one keystroke barely
// registers as a delay) and long enough to coalesce a burst of j/k
// repeats into a single fetch when the user holds the key down.
const detailFollowDebounce = 75 * time.Millisecond

// handleDetailFollowTick fires when the 75ms debounce window
// expires. Drops stale ticks (gen mismatch) so a coalesced burst
// only fires one fetch. The fetch uses m.detail.gen — the current
// detail-open generation — so an in-flight fetch from a still-newer
// cursor change can drop this one's response too via applyFetched's
// gen check.
func (m Model) handleDetailFollowTick(msg detailFollowTickMsg) (Model, tea.Cmd) {
	if msg.gen != m.nextDetailFollowGen {
		return m, nil
	}
	if m.api == nil || m.detail.issue == nil {
		return m, nil
	}
	pid := m.detail.scopePID
	ref := m.detail.issue.ShortID
	gen := m.detail.gen
	return m, tea.Batch(
		fetchIssue(m.api, pid, ref, gen),
		fetchComments(m.api, pid, ref, gen),
		fetchEvents(m.api, pid, ref, gen),
		fetchLinks(m.api, pid, ref, gen),
	)
}

// View returns the rendered string for the active sub-view. The list
// view consumes its own SSE state + toast inline (via the M1 chrome);
// other views still get the SSE/toast extras appended below since they
// don't carry a status line of their own. Both extras render as empty
// strings in the steady state so the view does not gain spurious blank
// lines.
func (m Model) View() string {
	// M5: degraded full-screen hint when the terminal is too narrow for
	// readable table columns. Short terminals still render the active view:
	// the list/detail renderers already have compact fallbacks, and blocking
	// on height makes tmux panes feel broken.
	//
	// Short-circuit BEFORE the regular dispatch so we don't render a
	// cropped/torn frame on narrow width. The `m.width > 0` gate avoids
	// tripping the hint before the first WindowSizeMsg arrives (initial
	// state has width=0). q / ctrl+c still route through routeGlobalKey, so
	// the user can quit from the hint without resizing first.
	//
	// Active modal overlays (quit-confirm, centered forms) layer on
	// top of the hint so a modal opened at full width stays visible
	// after a resize below threshold (roborev #250). Without this the
	// modal would silently disappear and the user would be stuck —
	// pressing q again would only re-trigger the (invisible) modal.
	if m.width > 0 && m.width < 80 {
		// viewProjects renders its own narrow-friendly body; every other
		// view falls back to the "too narrow" hint. Either way an active
		// modal/form must layer on top — without that, a quit-confirm
		// opened at full width would silently disappear under the
		// projects view after a resize below threshold (mirror of the
		// roborev #250 incident on the regular narrow path).
		var body string
		switch m.view {
		case viewProjects:
			body = renderProjects(m)
		default:
			body = renderTooNarrow(m.width, m.height)
		}
		body = m.appendNonListDetailExtras(body)
		if m.modal == modalQuitConfirm {
			return overlayModal(body, renderQuitConfirmModal(), m.width, m.height)
		}
		if m.input.kind.isCenteredForm() {
			form := renderCenteredForm(m.input, m.width, m.height)
			return overlayModal(body, form, m.width, m.height)
		}
		return body
	}
	body := m.viewBody()
	body = m.appendNonListDetailExtras(body)
	// M3.5b: a centered modal overlays the rendered view when active.
	if m.modal == modalQuitConfirm {
		return overlayModal(body, renderQuitConfirmModal(), m.width, m.height)
	}
	// M4: centered form overlays the rendered view when active.
	if m.input.kind.isCenteredForm() {
		form := renderCenteredForm(m.input, m.width, m.height)
		return overlayModal(body, form, m.width, m.height)
	}
	// Plan-8: label-prompt autocomplete menu overlays the detail view
	// above the info line. The detail layout reserves the menu's
	// rendered height when computing the tab/body budget — see
	// detail_render.go::renderInfoLine. M6: in split layout the
	// detail pane is always visible so the menu also fires when the
	// user is on focusDetail regardless of m.view.
	if m.detailIsActive() && isLabelPromptKind(m.input.kind) {
		body = m.overlaySuggestMenu(body)
	}
	return body
}

func (m Model) appendNonListDetailExtras(body string) string {
	if m.view != viewList && m.view != viewDetail {
		extras := []string{}
		if s := renderSSEStatus(m.sseStatus); s != "" {
			extras = append(extras, s)
		}
		if s := renderToast(m.toast); s != "" {
			extras = append(extras, s)
		}
		if len(extras) > 0 {
			body = joinNonEmpty(append([]string{body}, extras...))
		}
	}
	return body
}

// overlaySuggestMenu places the suggestion menu above the info line
// (info row = height-2; menu's bottom row = info-line row - 1, so
// the menu sits with its last bordered row one cell above the info
// line). Right-anchored: the menu's right edge meets the inner
// right edge of the panel. Returns body unchanged when the menu has
// no rows to render (defensive — placeholderRows always returns at
// least one).
//
// In M6 split layout, the detail pane occupies the right side of
// the screen; the menu's anchor stays right-edged but clamps to the
// detail-pane's column range so the menu floats over the detail
// pane (where the prompt is) rather than spilling over the list
// pane on the left.
func (m Model) overlaySuggestMenu(body string) string {
	suggestions := filterSuggestions(
		m.suggestionsForPrompt(m.input),
		m.activeBuffer(),
	)
	entry := m.cacheEntryForPrompt(m.input)
	menu := renderSuggestMenu(m.input, suggestions, entry)
	if menu == "" {
		return body
	}
	menuW := lipgloss.Width(menu)
	menuH := lipgloss.Height(menu)
	// Info line is at height-2 (footer is height-1). Menu's bottom
	// border lives one row above the info line, so the top row is
	// (height - 2) - menuH.
	anchorRow := m.height - 2 - menuH
	anchorCol := m.width - menuW - 1
	if m.layout == layoutSplit {
		// Anchor inside the detail pane only: the detail pane starts
		// at the column right after the list pane. The menu's left
		// edge must not encroach into the list pane.
		minCol := splitListPaneWidth(m.width) + 1
		if anchorCol < minCol {
			anchorCol = minCol
		}
	}
	return overlayAtCorner(body, menu, m.width, m.height, anchorRow, anchorCol)
}

// activeBuffer returns the current text in the active input field, or
// "" if no field is active. Used to project filterSuggestions's
// prefix so the suggestion menu narrows as the user types.
func (m Model) activeBuffer() string {
	if f := m.input.activeField(); f != nil {
		return f.value()
	}
	return ""
}

// cacheEntryForPrompt returns the labelCacheEntry the menu should
// reflect for s.kind == inputLabelPrompt. For inputRemoveLabelPrompt
// the entry's placeholder fields aren't consulted (suggestions come
// from dm.issue.Labels) so a zero entry is fine.
func (m Model) cacheEntryForPrompt(s inputState) labelCacheEntry {
	if s.kind != inputLabelPrompt || m.projectLabels == nil {
		return labelCacheEntry{}
	}
	return m.projectLabels.byProject[s.target.projectID]
}

// viewBody returns the active sub-view rendering. Splitting it off
// View keeps View's cyclomatic budget under the project limit.
//
// M6 split layout: list and detail render side-by-side when
// m.layout == layoutSplit. viewHelp / viewEmpty short-circuit ahead
// of the split path so the help overlay / onboarding hint always
// take the full screen regardless of layout.
func (m Model) viewBody() string {
	switch m.view {
	case viewHelp:
		return renderHelp(m.keymap, m.width, m.list.filter)
	case viewEmpty:
		return renderEmpty(m.width, m.height)
	case viewProjects:
		return renderProjects(m)
	case viewDaemons:
		return renderDaemons(m)
	}
	if m.layout == layoutSplit {
		return renderSplit(m)
	}
	switch m.view {
	case viewList:
		return m.list.View(m.width, m.height, m.chrome())
	case viewDetail:
		return m.detail.View(m.width, m.height, m.chrome())
	}
	return ""
}

// chrome assembles the cross-cutting render inputs both the list view
// and the detail view need from Model state: scope, SSE status,
// pending invalidation flag, the active toast (if any), the
// build-time version string, and the active input shell. Centralising
// this keeps the sub-views free of Model coupling.
func (m Model) chrome() viewChrome {
	return viewChrome{
		scope:        m.scope,
		sseStatus:    m.sseStatus,
		pending:      m.pendingRefetch,
		toast:        m.toast,
		version:      kataVersion,
		input:        m.input,
		projectsByID: m.projectsByID,
		daemon:       activeDaemonDisplay(m.activeDaemon),
	}
}

func activeDaemonDisplay(target daemonTarget) string {
	if target == (daemonTarget{}) {
		return ""
	}
	return daemonTargetDisplay(target)
}

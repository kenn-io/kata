package tui

import "time"

// initialFetchMsg is delivered after the first ListIssues call returns.
// dispatchKey captures the scope/filter at dispatch time so
// Model.populateCache can drop stale responses that arrive after a
// scope toggle or filter change.
type initialFetchMsg struct {
	connGen     uint64
	dispatchKey cacheKey
	issues      []Issue
	err         error
}

// refetchedMsg carries the result of a debounced or scope-change
// refetch. dispatchKey captures the scope/filter at dispatch time so
// Model.populateCache can drop stale responses — see initialFetchMsg.
type refetchedMsg struct {
	connGen     uint64
	dispatchKey cacheKey
	issues      []Issue
	err         error
}

// detailFetchedMsg carries the result of a single-issue refetch. It is
// produced by the Enter-jump path (Task 8) when a user navigates to a
// referenced issue, and will also be produced by the SSE-driven refetch
// in Task 11. gen tags the detail-open generation that dispatched the
// fetch — applyFetched discards messages whose gen no longer matches
// dm.gen so a fetch in flight when the user pops/jumps cannot pollute
// the new view with stale data.
type detailFetchedMsg struct {
	gen      int64
	issue    *Issue
	parent   *IssueRef
	children []Issue
	err      error
}

// commentsFetchedMsg, eventsFetchedMsg, and linksFetchedMsg carry the
// per-tab fetch results dispatched in parallel by openDetailMsg. gen
// is the detail-open generation; see detailFetchedMsg for the rationale.
type commentsFetchedMsg struct {
	gen      int64
	comments []CommentEntry
	err      error
}

type eventsFetchedMsg struct {
	gen    int64
	events []EventLogEntry
	err    error
}

type linksFetchedMsg struct {
	gen   int64
	links []LinkEntry
	err   error
}

// openDetailMsg is emitted by the list view when Enter selects a row.
// The top-level Model handles it: switches m.view to viewDetail, seeds
// m.detail.issue, and dispatches the three concurrent tab fetches.
type openDetailMsg struct {
	connGen uint64
	issue   Issue
}

// popDetailMsg reverts the top-level Model from viewDetail back to
// viewList. The list cursor and filter state are preserved because
// listModel is held by value and never reset on the round trip.
type popDetailMsg struct {
	connGen uint64
}

// openInputMsg asks the top-level Model to open an input shell of
// the given kind. Sub-views emit this rather than constructing the
// inputState directly so the centralised Model.Update routes the
// open through the shared input pipeline (snapshot/restore, focus
// gating, render integration).
//
// parentShortID is the new-child form's pre-filled parent ref: when
// non-nil, the form's Parent field is locked to this short_id so the
// user can't change the parentage after committing the keystroke.
type openInputMsg struct {
	kind          inputKind
	parentShortID *string
}

// jumpDetailMsg asks the top-level Model to jump the detail view to
// the named issue. detail.handleEnter emits this message rather than
// performing the jump itself so the new generation comes from
// Model.nextGen — a counter that monotonically increases across the
// model's lifetime, never reused even after handleBack restores an
// older detailModel snapshot from the navStack. Without the hoist,
// jumpTo's `dm.gen+1` could collide with an in-flight fetch from a
// previously-jumped issue once the user backs to the smaller-gen
// snapshot and re-jumps.
//
// ref is the short_id, qualified short_id, or UID accepted by the
// daemon's path resolver — the same wire shape that any client-side
// fetch path uses.
type jumpDetailMsg struct {
	connGen uint64
	ref     string
}

// mutationDoneMsg is the result of any single mutation (create now,
// close/reopen/label/owner in Task 9). kind names which mutation so the
// list/detail Update can route to the right post-success behavior.
//
// origin discriminates which view dispatched the mutation: "list"
// mutations land in listModel.applyMutation, "detail" mutations land in
// detailModel.applyMutation, "form" mutations land in routeFormMutation.
// Without this tag, a list-side close completing after the user opened
// detail (or a detail close that arrives after Esc) would route the
// response to the wrong view, churn the wrong status line, and (for
// detail) trigger an unwanted refetch.
//
// gen is the detail-open generation that dispatched the mutation, set
// only when origin == "detail". The detail Update path drops responses
// whose gen does not match dm.gen so a mutation in flight when the user
// jumps or pops cannot apply to the new view.
//
// formGen is the centered-form generation captured at dispatch time
// (set only when origin == "form"). routeFormMutation drops responses
// whose formGen does not match m.input.formGen — guards against a
// stale form-A response landing on a different form-B that the user
// opened in the meantime. Without this guard, the stale response could
// close form B, misroute as origin=detail, or fire an unrelated
// batchLabelRefresh against form A's project. Jobs 242/244.
type mutationDoneMsg struct {
	connGen uint64
	origin  string
	gen     int64
	formGen int64
	kind    string
	resp    *MutationResp
	err     error
}

// editorReturnedMsg carries the result of a $EDITOR suspend/resume
// cycle. kind discriminates which mutation should run on the trimmed
// content: "edit" (issue body) or "comment" (new comment). err is
// non-nil when the editor exited with a non-zero status or the
// tmpfile read-back failed.
//
// formGen is the M4 stale-handoff guard. The Model.openBodyEditForm /
// openCommentForm path stamps a monotonic ID into the form state and
// rides it through editorCmd. Routing checks the return's formGen
// against the currently-open form's formGen; a mismatch (form was
// closed, or re-opened on a different issue while the editor ran)
// drops the content rather than writing it into a different form's
// textarea. formGen=0 means "legacy editor return" and follows the
// pre-M4 detail-side dispatch path.
type editorReturnedMsg struct {
	kind, content string
	err           error
	formGen       int64
}

// eventReceivedMsg is the per-frame SSE message forwarded to the TEA
// loop by startSSE. issueShortID is empty when the event has no
// associated issue (project-level events) and issueUID is the canonical
// reference used for detail-pane matching.
//
// linksChanged is populated only for issue.links_changed events (the
// aggregated form emitted by `kata edit`). It carries parent_set and
// parent_removed from the payload so the detail-pane refetch logic
// can refresh the OTHER endpoint of a parent transition — without it,
// the old or new parent's pane would stay stale until a manual
// refresh after a parent replace.
type eventReceivedMsg struct {
	gen             uint64
	eventType       string
	projectID       int64
	projectUID      string
	issueShortID    string
	issueUID        string
	relatedIssueUID string
	link            *linkPayload
	linksChanged    *linksChangedParents
}

// linkPayload mirrors the per-link issue.linked / issue.unlinked
// envelope. UIDs are canonical; short_ids are display snapshots that
// may shift across a short_id cutover or federation merge. The daemon
// emits `from_uid` / `to_uid` on link events (see queries_links.go's
// CreateLinkAndEvent / DeleteLinkAndEvent); the issue.created path
// builds this struct field-by-field through parentLinkFromCreatedPayload
// so its wire shape (to_issue_uid) doesn't drive the tags here.
type linkPayload struct {
	Type         string `json:"type"`
	FromShortID  string `json:"from_short_id,omitempty"`
	ToShortID    string `json:"to_short_id,omitempty"`
	FromIssueUID string `json:"from_uid,omitempty"`
	ToIssueUID   string `json:"to_uid,omitempty"`
}

// linksChangedParents holds every peer issue referenced by an
// issue.links_changed payload. The detail-refetch logic uses RefUIDs
// to invalidate panes on the OTHER end of any add/remove. UIDs are
// authoritative; short_ids are display snapshots that may shift
// across cutovers, so matching on short_id alone can refresh the
// wrong pane.
//
// SetUID / RemovedUID remain populated for parent transitions
// (replace surfaces both at once) so callers that specifically care
// about the parent slot can short-circuit without scanning the full
// Refs slice. Set / Removed carry the parent short_id snapshot for
// rendering when the consumer also wants the display ref.
//
// RefUIDs and Refs run parallel (same length / order). The daemon's
// post-kata#1 events always carry the *_uids fields so UID-keyed
// matching is the canonical path.
type linksChangedParents struct {
	Set        string   // parent_set short_id; "" when absent
	SetUID     string   // parent_set_uid; "" when absent
	Removed    string   // parent_removed short_id; "" when absent
	RemovedUID string   // parent_removed_uid; "" when absent
	Refs       []string // every peer short_id in the payload
	RefUIDs    []string // peer UIDs parallel to Refs
}

// labelsFetchedMsg carries the result of an api.ListLabels call. pid
// names which project the labels are for; gen is the dispatch-time
// generation so handleLabelsFetched can drop stale responses (a
// later dispatch under the same pid bumps the cache's gen, and any
// older response arriving after must NOT overwrite the newer state).
type labelsFetchedMsg struct {
	connGen uint64
	pid     int64
	gen     int64
	labels  []LabelCount
	err     error
}

// projectsLoadedMsg is delivered after a /api/v1/projects fetch returns.
// The all-projects list view uses the projects map to prefix each row's
// title with the owning project's display name. The stats and idents
// maps are populated only by fetchProjectsWithStats; the boot
// fetchProjects cmd leaves them nil so callers can distinguish "names
// only" vs "with stats".
//
// gen carries the Model.projectsGen value captured at dispatch time. The
// projectsLoadedMsg handler clears m.projectsStale only when gen still
// matches m.projectsGen — guards against an SSE invalidation arriving
// while an older fetch is in flight. fetchProjects (the boot, no-stats
// variant) leaves gen=0 since it does not participate in the stale-flip
// race. Spec §6.3.
type projectsLoadedMsg struct {
	connGen  uint64
	projects map[int64]string
	idents   map[int64]string
	stats    map[int64]ProjectStatsSummary
	err      error
	gen      uint64
}

// resetRequiredMsg signals sync.reset_required: the daemon's purge
// gap means the consumer's cursor is too old. The TEA loop drops the
// cache and refetches from scratch.
//
// We deliberately don't carry reset_after_id on this message: the
// daemon's contract (see internal/api/events.go EventReset) is that
// EventID == ResetAfterID, so the SSE frame's id: line — which the
// consumer already uses to update its Last-Event-ID resume cursor — is
// the authoritative checkpoint. A second copy of the same value on the
// envelope would invite drift if either path lagged.
type resetRequiredMsg struct{ gen uint64 }

// sseStatusMsg carries connection-state transitions from the SSE
// goroutine to the TEA loop so the status bar can render the
// reconnect indicator.
type sseStatusMsg struct {
	gen   uint64
	state sseConnState
}

type daemonSwitchResultMsg struct {
	attempt uint64
	conn    daemonConnection
	target  daemonTarget
	err     error
}

// sseConnState is the SSE consumer's connection state.
type sseConnState int

const (
	sseConnected sseConnState = iota
	sseReconnecting
	sseDisconnected
)

// refetchTickMsg fires after the 150ms debounce window so a single
// fetch covers a burst of events.
type refetchTickMsg struct{}

// detailFollowTickMsg fires after the 75ms M6 split-mode debounce
// window so a burst of cursor moves coalesces into one detail fetch.
// gen is the dispatch-time Model.nextDetailFollowGen value;
// handleDetailFollowTick drops a tick whose gen no longer matches
// m.nextDetailFollowGen — meaning the user has moved the cursor
// again and a fresher tick is in flight.
type detailFollowTickMsg struct {
	gen int64
}

// toastExpiredMsg fires after a toast's TTL so Update can clear it.
type toastExpiredMsg struct{}

// toast is a transient status notification rendered below the active
// view. Task 11 uses it for the 'resynced' notice; Task 12 will own
// stacked toasts for mutation feedback.
type toast struct {
	text      string
	level     toastLevel
	expiresAt time.Time
}

// toastLevel discriminates toast styling.
type toastLevel int

const (
	toastInfo toastLevel = iota
	//nolint:unused // reserved for mutation success toasts (deferred to Task 14)
	toastSuccess
	toastError
)

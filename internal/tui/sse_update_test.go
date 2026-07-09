package tui

import (
	"errors"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sseUpdateFixture builds a minimal Model wired for the SSE Update-side
// handler tests. sseCh is nil so waitForSSE returns nil; that way the
// returned tea.Cmd shape is the handler's contribution alone (a tick or
// nil), not noise from re-arming the SSE bridge. cache is allocated so
// markStale doesn't nil-panic. toastNow is fixed so toast-expiry tests
// have a deterministic clock to drive.
func sseUpdateFixture() Model {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return Model{
		view:     viewList,
		cache:    newIssueCache(),
		toastNow: func() time.Time { return now },
	}
}

// sseUpdateFixtureAt builds a fixture whose toastNow returns t — used by
// the toast-expiry tests so the wall-clock check can be driven both
// before and after a toast's expiresAt.
func sseUpdateFixtureAt(t time.Time) Model {
	m := sseUpdateFixture()
	m.toastNow = func() time.Time { return t }
	return m
}

// sseDetailFixture builds a Model in detail view focused on a specific
// open issue, with a stub *Client wired so refetch helpers can capture
// it without hitting the network. detail.gen is seeded non-zero so
// generation-tagged fetches don't look like the zero-valued initial
// state. issueShortID and issueUID identify the open issue — both are
// load-bearing for the UID-keyed peer-match path.
func sseDetailFixture(projectID int64, issueShortID, issueUID string) Model {
	m := sseUpdateFixture()
	m.scope = scope{projectID: projectID}
	m.api = NewClient("http://kata.invalid", nil)
	m.view = viewDetail
	m.detail.issue = &Issue{
		ProjectID: projectID,
		ShortID:   issueShortID,
		UID:       issueUID,
		Status:    "open",
	}
	m.detail.scopePID = projectID
	m.detail.gen = 5
	return m
}

// assertDetailRefetchBatch fails t unless cmd produces a tea.BatchMsg
// containing exactly four fetches (issue + comments + events + links) —
// the shape every detail-tab refetch must dispatch.
func assertDetailRefetchBatch(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected non-nil cmd, got nil")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected tea.BatchMsg, got %T", msg)
	}
	if got := len(batch); got != 4 {
		t.Fatalf("expected 4 fetches in batch (issue + 3 tabs), got %d", got)
	}
}

// TestEventAffectsView_AllProjects: in all-projects scope, any event
// with a non-zero projectID affects the view; projectID == 0 does not.
func TestEventAffectsView_AllProjects(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{allProjects: true}
	if !m.eventAffectsView(eventReceivedMsg{projectID: 1}) {
		t.Fatal("projectID=1 in all-projects scope must affect view")
	}
	if !m.eventAffectsView(eventReceivedMsg{projectID: 999}) {
		t.Fatal("projectID=999 in all-projects scope must affect view")
	}
	if m.eventAffectsView(eventReceivedMsg{projectID: 0}) {
		t.Fatal("projectID=0 must not affect view (system-wide ignore)")
	}
}

// TestEventAffectsView_SingleProject: in single-project scope, only the
// matching projectID affects the view; other projects do not.
func TestEventAffectsView_SingleProject(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	if !m.eventAffectsView(eventReceivedMsg{projectID: 7}) {
		t.Fatal("matching projectID must affect view")
	}
	if m.eventAffectsView(eventReceivedMsg{projectID: 8}) {
		t.Fatal("non-matching projectID must not affect view")
	}
}

// TestEventAffectsView_ZeroProjectID_SingleScope: locks in the chosen
// behavior — projectID==0 is treated as a system-wide event we ignore
// regardless of scope, so the daemon can broadcast unscoped frames
// without churning a single-project view.
func TestEventAffectsView_ZeroProjectID_SingleScope(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	if m.eventAffectsView(eventReceivedMsg{projectID: 0}) {
		t.Fatal("projectID=0 must not affect single-project view")
	}
}

// TestHandleEventReceived_DispatchesDebouncedRefetch: a fresh
// affects-view event flips pendingRefetch and returns a non-nil cmd
// (the 150ms tick). The cache, primed with a put so isStale's set+stale
// gate is meaningful, is marked stale so the tick's eventual refetch
// path will run.
func TestHandleEventReceived_DispatchesDebouncedRefetch(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	m.cache.put(cacheKey{projectID: 7}, []Issue{{ShortID: "aaa1"}})
	out, cmd := m.handleEventReceived(eventReceivedMsg{projectID: 7})
	mm := out.(Model)
	if !mm.pendingRefetch {
		t.Fatal("pendingRefetch must be true after first affects-view event")
	}
	if !mm.cache.isStale() {
		t.Fatal("cache must be marked stale (set+stale gate)")
	}
	if cmd == nil {
		t.Fatal("cmd must be non-nil (the debounce tick)")
	}
}

// TestHandleEventReceived_CoalescesBursts: three back-to-back
// affects-view events coalesce — pendingRefetch stays true and only
// the first dispatch returns a non-nil cmd.
func TestHandleEventReceived_CoalescesBursts(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	out, cmd1 := m.handleEventReceived(eventReceivedMsg{projectID: 7})
	m = out.(Model)
	out, cmd2 := m.handleEventReceived(eventReceivedMsg{projectID: 7})
	m = out.(Model)
	out, cmd3 := m.handleEventReceived(eventReceivedMsg{projectID: 7})
	m = out.(Model)
	if cmd1 == nil {
		t.Fatal("first cmd must be non-nil (the tick)")
	}
	if cmd2 != nil {
		t.Fatalf("second cmd must coalesce to nil, got %T", cmd2)
	}
	if cmd3 != nil {
		t.Fatalf("third cmd must coalesce to nil, got %T", cmd3)
	}
	if !m.pendingRefetch {
		t.Fatal("pendingRefetch must remain true through the burst")
	}
}

// TestHandleEventReceived_NoEffect_NoStale: an event for a different
// project in single-project scope leaves the cache untouched and does
// not flip pendingRefetch.
func TestHandleEventReceived_NoEffect_NoStale(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	out, cmd := m.handleEventReceived(eventReceivedMsg{projectID: 8})
	mm := out.(Model)
	if mm.pendingRefetch {
		t.Fatal("pendingRefetch must stay false for non-affecting event")
	}
	if mm.cache.isStale() {
		t.Fatal("cache must not be marked stale for non-affecting event")
	}
	if cmd != nil {
		t.Fatalf("cmd must be nil (no work), got %T", cmd)
	}
}

// TestHandleEventReceived_DetailViewSingleIssueRefetch: when the user
// is in detail-view and the event names dm.issue (matched by UID),
// maybeRefetchOpenDetail returns a non-nil cmd (the batch of four
// fetches: issue + comments + events + links). We test the helper
// directly so we don't have to invoke a 150ms tick to assert on cmd
// shape.
func TestHandleEventReceived_DetailViewSingleIssueRefetch(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		projectID: 7, issueShortID: "abc4", issueUID: "01UID-OPEN",
	})
	if cmd == nil {
		t.Fatal("maybeRefetchOpenDetail must return a fetch cmd for matching issue")
	}
	// And the parent handler still reports pendingRefetch=true and a
	// non-nil cmd (the tick + the four-tab fetch batch, batched).
	out, parentCmd := m.handleEventReceived(eventReceivedMsg{
		projectID: 7, issueShortID: "abc4", issueUID: "01UID-OPEN",
	})
	mm := out.(Model)
	if !mm.pendingRefetch {
		t.Fatal("pendingRefetch must be set after detail-match event")
	}
	if parentCmd == nil {
		t.Fatal("handleEventReceived must return a non-nil cmd batch")
	}
}

func TestHandleEventReceived_ParentLinkInvalidatesQueue(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	m.cache.put(cacheKey{projectID: 7}, []Issue{{ShortID: "abc4"}})
	out, cmd := m.handleEventReceived(eventReceivedMsg{
		eventType: "issue.linked",
		projectID: 7,
		link: &linkPayload{
			Type: "parent", FromShortID: "ch43", ToShortID: "abc4",
			FromIssueUID: "01UID-CHILD", ToIssueUID: "01UID-PARENT",
		},
	})
	nm := out.(Model)
	if !nm.cache.isStale() {
		t.Fatal("parent link event must mark queue cache stale")
	}
	if cmd == nil {
		t.Fatal("parent link event must schedule queue refetch")
	}
}

func TestHandleEventReceived_ParentLinkRefetchesOpenParentDetail(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-PARENT")

	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.linked",
		projectID:    7,
		issueShortID: "ch43",
		issueUID:     "01UID-CHILD",
		link: &linkPayload{
			Type: "parent", FromShortID: "ch43", ToShortID: "abc4",
			FromIssueUID: "01UID-CHILD", ToIssueUID: "01UID-PARENT",
		},
	})
	if cmd == nil {
		t.Fatal("parent detail must refetch when a child is linked to it")
	}
}

func TestHandleEventReceived_ParentLinkRefetchesOpenChildDetail(t *testing.T) {
	m := sseDetailFixture(7, "ch43", "01UID-CHILD")

	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.linked",
		projectID:    7,
		issueShortID: "abc4",
		issueUID:     "01UID-PARENT",
		link: &linkPayload{
			Type: "parent", FromShortID: "ch43", ToShortID: "abc4",
			FromIssueUID: "01UID-CHILD", ToIssueUID: "01UID-PARENT",
		},
	})
	if cmd == nil {
		t.Fatal("child detail must refetch when its parent link changes")
	}
}

// TestHandleEventReceived_LinksChangedRefetchesNewParent covers the
// `kata edit --parent X` path: an issue.links_changed event with
// parent_set must refresh the new parent's detail pane when it's open.
func TestHandleEventReceived_LinksChangedRefetchesNewParent(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-PARENT")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.links_changed",
		projectID:    7,
		issueShortID: "ch43",
		issueUID:     "01UID-CHILD",
		linksChanged: &linksChangedParents{Set: "abc4", SetUID: "01UID-PARENT"},
	})
	if cmd == nil {
		t.Fatal("new-parent detail must refetch when a child sets parent via links_changed")
	}
}

// TestHandleEventReceived_LinksChangedRefetchesOldAndNewParents covers
// the parent-replace case: issue.links_changed carries both parent_set
// and parent_removed, and either's pane (when open) should refresh.
func TestHandleEventReceived_LinksChangedRefetchesOldAndNewParents(t *testing.T) {
	cases := []struct {
		openShortID string
		openUID     string
	}{
		{"abc4", "01UID-NEW-PARENT"},
		{"prev9", "01UID-OLD-PARENT"},
	}
	for _, tc := range cases {
		t.Run(tc.openShortID, func(t *testing.T) {
			m := sseDetailFixture(7, tc.openShortID, tc.openUID)
			cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
				eventType:    "issue.links_changed",
				projectID:    7,
				issueShortID: "ch43",
				issueUID:     "01UID-CHILD",
				linksChanged: &linksChangedParents{
					Set: "abc4", SetUID: "01UID-NEW-PARENT",
					Removed: "prev9", RemovedUID: "01UID-OLD-PARENT",
				},
			})
			if cmd == nil {
				t.Fatalf("detail #%s must refetch when its end of a parent transition is touched", tc.openShortID)
			}
		})
	}
}

// TestHandleEventReceived_LinksChangedRefetchesBlocksTarget covers the
// iteration-11 fix carried over: non-parent link mutations (blocks,
// blocked_by, related) must also drive an other-endpoint refetch.
// Without scanning the full Refs slice, a `kata edit X --blocks Y`
// would refresh X's pane only — Y's pane would stay stale until a
// manual refresh.
func TestHandleEventReceived_LinksChangedRefetchesBlocksTarget(t *testing.T) {
	cases := []struct {
		openShortID string
		openUID     string
	}{
		{"f005", "01UID-FIRST"},
		{"f006", "01UID-SECOND"},
	}
	for _, tc := range cases {
		t.Run(tc.openShortID, func(t *testing.T) {
			m := sseDetailFixture(7, tc.openShortID, tc.openUID)
			cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
				eventType:    "issue.links_changed",
				projectID:    7,
				issueShortID: "ch43",
				issueUID:     "01UID-OTHER",
				linksChanged: &linksChangedParents{
					Refs:    []string{"f005", "f006"},
					RefUIDs: []string{"01UID-FIRST", "01UID-SECOND"},
				},
			})
			if cmd == nil {
				t.Fatalf("blocks target #%s must refetch on links_changed", tc.openShortID)
			}
		})
	}
}

// TestHandleEventReceived_IssueCreatedRefreshesNonParentPeer covers
// an iteration-14 finding (now baked in): a `kata create` with
// --blocked-by or --related folds those links into the issue.created
// payload; the SSE decoder surfaces every peer in the payload's links
// array so any open pane on the other end refreshes.
func TestHandleEventReceived_IssueCreatedRefreshesNonParentPeer(t *testing.T) {
	// Detail pane is on `peer5` — the peer of a `--related peer5` create.
	m := sseDetailFixture(7, "peer5", "01UID-PEER")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.created",
		projectID:    7,
		issueShortID: "new9",
		issueUID:     "01UID-NEW",
		linksChanged: &linksChangedParents{
			Refs:    []string{"peer5"},
			RefUIDs: []string{"01UID-PEER"},
		},
	})
	if cmd == nil {
		t.Fatal("peer detail must refetch when another issue creates a link to it")
	}
}

// TestHandleEventReceived_LinksChangedUIDMismatchSameShortID pins
// UID-aware peer matching: short_ids can collide across federation
// merge, so a UID mismatch beats a short_id match. An event that
// references peer UID-B at short_id "peer5" must NOT refresh an open
// detail pane on UID-A also at "peer5".
func TestHandleEventReceived_LinksChangedUIDMismatchSameShortID(t *testing.T) {
	m := sseDetailFixture(7, "peer5", "01UID-A")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.links_changed",
		projectID:    7,
		issueShortID: "ch43",
		issueUID:     "01UID-CHILD",
		linksChanged: &linksChangedParents{
			Refs:    []string{"peer5"},
			RefUIDs: []string{"01UID-B"},
		},
	})
	if cmd != nil {
		t.Fatal("must not refetch when peer UID mismatches even on short_id collision")
	}
}

// TestHandleEventReceived_LinksChangedUIDMatchesAuthoritatively pairs
// with the mismatch test above: when the event carries the watched
// detail's UID, the pane refreshes regardless of whether the
// short_id-keyed Refs slice matches.
func TestHandleEventReceived_LinksChangedUIDMatchesAuthoritatively(t *testing.T) {
	m := sseDetailFixture(7, "peer5", "01UID-B")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.links_changed",
		projectID:    7,
		issueShortID: "ch43",
		issueUID:     "01UID-CHILD",
		linksChanged: &linksChangedParents{RefUIDs: []string{"01UID-B"}},
	})
	if cmd == nil {
		t.Fatal("must refetch when peer UID matches detail UID")
	}
}

// TestHandleEventReceived_LinksChangedFallsBackToShortIDWhenNoUIDs
// covers the legacy / synthetic-event path: a linksChangedParents
// constructed without RefUIDs still drives a refetch via the
// short_id-only Refs slice. The daemon's post-kata#1 events always
// carry UIDs, so this is mainly belt-and-suspenders for hand-built
// fixtures.
func TestHandleEventReceived_LinksChangedFallsBackToShortIDWhenNoUIDs(t *testing.T) {
	m := sseDetailFixture(7, "peer5", "01UID-DETAIL")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.links_changed",
		projectID:    7,
		issueShortID: "ch43",
		issueUID:     "01UID-CHILD",
		linksChanged: &linksChangedParents{Refs: []string{"peer5"}},
	})
	if cmd == nil {
		t.Fatal("must refetch by short_id when event carries no peer UIDs")
	}
}

// TestHandleEventReceived_LinksChangedChildSelfRefetches covers
// `kata edit --remove-parent X`: the child issue's own detail must
// refresh because issueUID == openUID (the parent_removed payload is
// informational; the URL-issue match drives the refetch).
func TestHandleEventReceived_LinksChangedChildSelfRefetches(t *testing.T) {
	m := sseDetailFixture(7, "ch43", "01UID-CHILD")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.links_changed",
		projectID:    7,
		issueShortID: "ch43",
		issueUID:     "01UID-CHILD",
		linksChanged: &linksChangedParents{Removed: "prev9", RemovedUID: "01UID-OLD-PARENT"},
	})
	if cmd == nil {
		t.Fatal("child detail must refetch on parent removal via links_changed")
	}
}

// TestHandleEventReceived_IssueCreatedWithParentRefetchesOpenParent
// covers the agent-creates-subissue path: the daemon's CreateIssue
// folds a parent link into a single issue.created event (no separate
// issue.linked emit, see internal/db/queries.go::buildCreatedPayload),
// so the SSE handler must recognize an issue.created event whose
// payload carries a parent link and refetch the open parent's detail
// — otherwise the parent's children section stays stale until reload.
func TestHandleEventReceived_IssueCreatedWithParentRefetchesOpenParent(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-PARENT")

	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.created",
		projectID:    7,
		issueShortID: "new9",
		issueUID:     "01UID-NEW",
		link: &linkPayload{
			Type:         "parent",
			FromShortID:  "new9",
			FromIssueUID: "01UID-NEW",
			ToShortID:    "abc4",
			ToIssueUID:   "01UID-PARENT",
		},
	})
	if cmd == nil {
		t.Fatal("parent detail must refetch when a child is created with a parent link")
	}
}

// TestSSEUpdate_ReadsIssueShortID pins that the SSE decoder reads the
// `issue_short_id` field of an event envelope and exposes it as
// issueShortID on the resulting eventReceivedMsg. UID rides alongside
// for canonical matching.
func TestSSEUpdate_ReadsIssueShortID(t *testing.T) {
	body := []byte(`{
		"type":"issue.created",
		"project_id":7,
		"project_uid":"01JZ0000000000000000000002",
		"issue_short_id":"d4ex",
		"issue_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX"
	}`)
	got := decodeEventReceived(frame{kind: frameEvent, eventType: "issue.created", data: body})
	if got.eventType != "issue.created" {
		t.Fatalf("eventType = %q, want issue.created", got.eventType)
	}
	if got.issueShortID != "d4ex" {
		t.Fatalf("issueShortID = %q, want d4ex", got.issueShortID)
	}
	if got.issueUID != "01HZNQ7VFPK1XGD8R5MABCD4EX" {
		t.Fatalf("issueUID = %q, want the full ULID", got.issueUID)
	}
}

// TestDecodeEventReceived_IssueCreatedExtractsParentLink covers the
// payload-extraction half: the SSE parser must surface the embedded
// parent link out of the issue.created payload so the dispatcher can
// match it against the open detail. Mirror of the issue.linked test
// (sse_test.go) but for the issue.created shape the agent path emits.
func TestDecodeEventReceived_IssueCreatedExtractsParentLink(t *testing.T) {
	body := []byte(`{
		"type":"issue.created",
		"project_id":7,
		"issue_short_id":"new9",
		"issue_uid":"01UID-NEW",
		"payload":{"links":[{"type":"parent","to_short_id":"abc4","to_issue_uid":"01UID-PARENT"}]}
	}`)
	got := decodeEventReceived(frame{eventType: "issue.created", data: body})
	if got.eventType != "issue.created" {
		t.Fatalf("eventType = %q, want issue.created", got.eventType)
	}
	if got.link == nil {
		t.Fatal("expected parent link extracted from payload, got nil")
	}
	if got.link.Type != "parent" {
		t.Errorf("link.Type = %q, want parent", got.link.Type)
	}
	if got.link.ToShortID != "abc4" {
		t.Errorf("link.ToShortID = %q, want abc4", got.link.ToShortID)
	}
	if got.link.ToIssueUID != "01UID-PARENT" {
		t.Errorf("link.ToIssueUID = %q, want the parent's UID", got.link.ToIssueUID)
	}
	// from_* is implicit (the new issue) — fall back to the issue's own ref.
	if got.link.FromShortID != "new9" {
		t.Errorf("link.FromShortID = %q, want new9", got.link.FromShortID)
	}
	if got.link.FromIssueUID != "01UID-NEW" {
		t.Errorf("link.FromIssueUID = %q, want 01UID-NEW", got.link.FromIssueUID)
	}
}

func TestHandleEventReceived_NonParentLinkDoesNotRefetchForHierarchy(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")

	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.linked",
		projectID:    7,
		issueShortID: "new9",
		issueUID:     "01UID-NEW",
		link: &linkPayload{
			Type:         "blocks",
			FromShortID:  "ch43",
			ToShortID:    "abc4",
			FromIssueUID: "01UID-CHILD",
			ToIssueUID:   "01UID-OPEN",
		},
	})
	if cmd != nil {
		t.Fatalf("non-parent link should not refetch for hierarchy, got %T", cmd)
	}
}

// TestHandleEventReceived_DetailViewRefetchesAllTabs: a matching SSE
// event must batch the four detail fetches (issue + comments + events
// + links) so every tab is refreshed regardless of event-kind. Earlier
// the helper only refetched GetIssue, leaving comments/events/links
// stale on issue.commented / issue.linked / issue.relabeled.
//
// We assert the cmd batch shape (4 children) rather than invoking the
// children: maybeRefetchOpenDetail uses m.api (a real *Client), so
// driving the children would actually hit the network.
func TestHandleEventReceived_DetailViewRefetchesAllTabs(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")

	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		projectID: 7, issueShortID: "abc4", issueUID: "01UID-OPEN",
	})
	assertDetailRefetchBatch(t, cmd)
}

// TestHandleEventReceived_DetailViewMismatch_NoRefetch: an event with a
// different issue ref than the open detail issue must not trigger a
// detail refetch — maybeRefetchOpenDetail returns nil. Tested directly
// to avoid invoking the 150ms debounce tick.
func TestHandleEventReceived_DetailViewMismatch_NoRefetch(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		projectID: 7, issueShortID: "xy99", issueUID: "01UID-OTHER",
	})
	if cmd != nil {
		t.Fatalf("maybeRefetchOpenDetail must return nil for non-matching ref, got %T", cmd)
	}
}

// TestHandleEventReceived_CrossProjectMismatch_NoRefetch: in all-
// projects scope, short_ids are project-scoped — project A's abc4 is
// not project B's abc4. An event for project B abc4 must NOT trigger a
// refetch of the open project A abc4 detail.
func TestHandleEventReceived_CrossProjectMismatch_NoRefetch(t *testing.T) {
	// Open detail is project A (abc4); event is project B (abc4).
	m := sseDetailFixture(7, "abc4", "01UID-A")
	m.scope = scope{allProjects: true}
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		projectID: 8, issueShortID: "abc4", issueUID: "01UID-B",
	})
	if cmd != nil {
		t.Fatalf("cross-project event with same short_id must not refetch, got %T", cmd)
	}
}

// TestMaybeRefetchOpenDetail_ListView_NoRefetch: even with a matching
// short_id, list-view (not detail) must not dispatch a refetch.
func TestMaybeRefetchOpenDetail_ListView_NoRefetch(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m.view = viewList
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		projectID: 7, issueShortID: "abc4", issueUID: "01UID-OPEN",
	})
	if cmd != nil {
		t.Fatalf("list-view must not refetch detail, got %T", cmd)
	}
}

// TestRefetchOpenDetail_BatchShape: when the user is in detail view,
// refetchOpenDetail returns a 4-fetch batch (issue + comments + events
// + links). Tested directly so we don't have to invoke the children
// (each calls into m.api with the real *Client and would hit the
// network).
func TestRefetchOpenDetail_BatchShape(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")

	assertDetailRefetchBatch(t, m.refetchOpenDetail())
}

// TestRefetchOpenDetail_NoOpInList: when the active view is the list,
// refetchOpenDetail must return nil so reset_required doesn't dispatch
// stale detail fetches over the wire. A leftover m.detail.issue from
// a prior open must NOT trigger a refetch.
func TestRefetchOpenDetail_NoOpInList(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	m.view = viewList

	if cmd := m.refetchOpenDetail(); cmd != nil {
		t.Fatalf("expected nil cmd in viewList, got %T", cmd)
	}
}

// TestRefetchOpenDetail_NoOpWithoutIssue: a fresh detailModel (no
// issue seeded) returns nil so the gen-tagged fetches don't fire
// against a zero-valued projectID/ref.
func TestRefetchOpenDetail_NoOpWithoutIssue(t *testing.T) {
	m := sseDetailFixture(7, "abc4", "01UID-OPEN")
	// view is viewDetail but pre-fetch — clear the seeded issue.
	m.detail.issue = nil
	if cmd := m.refetchOpenDetail(); cmd != nil {
		t.Fatalf("expected nil cmd when issue not seeded, got %T", cmd)
	}
}

// TestHandleResetRequired_DropsCacheAndShowsToast: a reset frame drops
// the cache, clears pendingRefetch, and seeds a "resynced" toast with
// a 2s expiry from toastNow.
func TestHandleResetRequired_DropsCacheAndShowsToast(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	m.cache.put(cacheKey{projectID: 7}, []Issue{{ShortID: "aaa1"}})
	m.cache.markStale()
	m.pendingRefetch = true
	out, _ := m.handleResetRequired(resetRequiredMsg{})
	mm := out.(Model)
	if mm.cache.set {
		t.Fatal("cache must be empty after reset")
	}
	if mm.cache.isStale() {
		t.Fatal("cache must not be marked stale after reset (it's empty)")
	}
	if mm.pendingRefetch {
		t.Fatal("pendingRefetch must be cleared after reset")
	}
	if mm.toast == nil {
		t.Fatal("toast must be set after reset")
	}
	if mm.toast.text != "resynced" {
		t.Fatalf("toast.text = %q, want %q", mm.toast.text, "resynced")
	}
	want := mm.toastNow().Add(toastResyncedTTL)
	if !mm.toast.expiresAt.Equal(want) {
		t.Fatalf("toast.expiresAt = %v, want %v", mm.toast.expiresAt, want)
	}
}

// TestHandleRefetchTick_ClearsPendingAndDispatchesIfStale: with stale
// cache and pendingRefetch=true, the tick clears pendingRefetch and
// dispatches a refetch (cmd is non-nil). We use a real *Client because
// list.refetchCmd captures it; the lazy cmd is never invoked.
func TestHandleRefetchTick_ClearsPendingAndDispatchesIfStale(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	m.api = NewClient("http://kata.invalid", nil)
	m.cache.put(cacheKey{projectID: 7}, []Issue{{ShortID: "aaa1"}})
	m.cache.markStale()
	m.pendingRefetch = true
	out, cmd := m.handleRefetchTick()
	mm := out.(Model)
	if mm.pendingRefetch {
		t.Fatal("pendingRefetch must be cleared after tick")
	}
	if cmd == nil {
		t.Fatal("cmd must be non-nil when cache is stale")
	}
}

// TestHandleRefetchTick_NoOpIfNotStale: with pendingRefetch=true but a
// fresh cache (e.g., a manual filter change just refetched), the tick
// clears pendingRefetch and returns nil — we don't spin a redundant
// fetch.
func TestHandleRefetchTick_NoOpIfNotStale(t *testing.T) {
	m := sseUpdateFixture()
	m.scope = scope{projectID: 7}
	m.cache.put(cacheKey{projectID: 7}, []Issue{{ShortID: "aaa1"}})
	m.pendingRefetch = true
	out, cmd := m.handleRefetchTick()
	mm := out.(Model)
	if mm.pendingRefetch {
		t.Fatal("pendingRefetch must be cleared after tick")
	}
	if cmd != nil {
		t.Fatalf("cmd must be nil when cache is fresh, got %T", cmd)
	}
}

// TestHandleToastExpired_ClearsToast: with toastNow >= expiresAt, the
// toast clears.
func TestHandleToastExpired_ClearsToast(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	m := sseUpdateFixtureAt(now)
	m.toast = &toast{
		text:      "resynced",
		level:     toastInfo,
		expiresAt: now.Add(-time.Second), // already expired
	}
	out, _ := m.handleToastExpired()
	mm := out.(Model)
	if mm.toast != nil {
		t.Fatalf("toast must be cleared, got %+v", mm.toast)
	}
}

// TestHandleToastExpired_PreservesNewerToast: a fresher toast (expiresAt
// in the future relative to toastNow) must NOT be cleared by a stale
// expiry tick. This guards against a sequence like reset_required → 2s
// later toastExpired arrives → user already replaced the toast with a
// fresher one whose expiry hasn't fired yet.
func TestHandleToastExpired_PreservesNewerToast(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	m := sseUpdateFixtureAt(now)
	fresher := &toast{
		text:      "fresher",
		level:     toastInfo,
		expiresAt: now.Add(2 * time.Second), // still in the future
	}
	m.toast = fresher
	out, _ := m.handleToastExpired()
	mm := out.(Model)
	if mm.toast == nil {
		t.Fatal("fresher toast must not be cleared by stale tick")
	}
	if mm.toast.text != "fresher" {
		t.Fatalf("toast.text = %q, want fresher", mm.toast.text)
	}
}

// TestProjectsView_StaleOnIssueEvent pins spec §6.3: an issue event for
// a project the table is showing flips m.projectsStale and dispatches
// the debounce timer. The stale-flip also bumps m.projectsGen so an
// in-flight fetch with the older gen cannot clear stale on response.
func TestProjectsView_StaleOnIssueEvent(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewProjects
	m.projectsByID = map[int64]string{7: "kata"}
	startGen := m.projectsGen

	out, cmd := m.Update(eventReceivedMsg{eventType: "issue.created", projectID: 7})
	nm := out.(Model)
	assert.True(t, nm.projectsStale)
	assert.True(t, nm.projectsRefetchPending)
	assert.Equal(t, startGen+1, nm.projectsGen, "stale-flip bumps gen")
	require.NotNil(t, cmd, "first event must dispatch a debounce timer")
}

// TestProjectsView_IgnoresEventsWhenInactive pins that the same event
// is a no-op when viewList is active — the next P-into-viewProjects
// transition does its own refetch. Spec §6.3.
func TestProjectsView_IgnoresEventsWhenInactive(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewList
	m.projectsByID = map[int64]string{7: "kata"}

	out, _ := m.Update(eventReceivedMsg{eventType: "issue.created", projectID: 7})
	nm := out.(Model)
	assert.False(t, nm.projectsStale)
	assert.False(t, nm.projectsRefetchPending)
}

// TestProjectsView_DebouncesRefetch pins that a burst of SSE events
// flips projectsStale once and dispatches exactly one debounce timer
// (no thundering herd). Spec §6.3.
func TestProjectsView_DebouncesRefetch(t *testing.T) {
	m := sseUpdateFixture()
	m.view = viewProjects
	m.projectsByID = map[int64]string{7: "kata"}

	var debounceCmds int
	for i := 0; i < 3; i++ {
		out, cmd := m.Update(eventReceivedMsg{eventType: "issue.created", projectID: 7})
		m = out.(Model)
		if cmd != nil {
			debounceCmds++
		}
	}
	assert.True(t, m.projectsStale)
	assert.True(t, m.projectsRefetchPending)
	// sseUpdateFixture has sseCh=nil, so waitForSSE returns nil. The
	// only non-nil cmd over the burst is the single debounce scheduled
	// by the first event.
	assert.Equal(t, 1, debounceCmds, "exactly one debounce timer")
}

// TestProjectsView_StaleOnUnknownProjectEvent pins that an event for a
// projectID NOT in m.projectsByID still flips projectsStale and
// schedules the debounce refetch. The unknown projectID is exactly the
// signal that a new project has appeared (e.g. `kata init` ran in
// another terminal); without this refresh, the all-projects table
// would never learn about it until the user manually refetched.
func TestProjectsView_StaleOnUnknownProjectEvent(t *testing.T) {
	m := sseUpdateFixture()
	m.view = viewProjects
	m.projectsByID = map[int64]string{7: "kata"}

	out, cmd := m.Update(eventReceivedMsg{eventType: "issue.created", projectID: 99})
	nm := out.(Model)
	assert.True(t, nm.projectsStale)
	assert.True(t, nm.projectsRefetchPending)
	require.NotNil(t, cmd, "unknown-project event must schedule a debounced refetch")
}

// TestProjectsDebounceFire_DispatchesFetchWhenActive pins that the
// debounce timer's wakeup dispatches fetchProjectsWithStats when the
// user is still in viewProjects and the stale flag is set. The flag
// is NOT cleared at dispatch — a failed fetch must leave the flag
// armed so the next debounce can retry. The flag is cleared by
// projectsLoadedMsg when the fetch lands successfully. Spec §6.3.
func TestProjectsDebounceFire_DispatchesFetchWhenActive(t *testing.T) {
	m := sseUpdateFixture()
	m.view = viewProjects
	m.projectsStale = true
	m.projectsRefetchPending = true
	m.api = &Client{}

	out, cmd := m.Update(projectsDebounceFireMsg{})
	nm := out.(Model)
	assert.False(t, nm.projectsRefetchPending, "pending flag must clear on fire")
	assert.True(t, nm.projectsStale, "stale persists until fetch lands")
	require.NotNil(t, cmd, "active view + stale → fetch must dispatch")
}

// TestProjectsDebounceFire_NoFetchWhenInactive pins that the timer's
// wakeup is a no-op for the fetch when the user has navigated away
// from viewProjects, but still clears the pending flag so future
// invalidations can re-arm. Spec §6.3.
func TestProjectsDebounceFire_NoFetchWhenInactive(t *testing.T) {
	m := sseUpdateFixture()
	m.view = viewList // user navigated away
	m.projectsStale = true
	m.projectsRefetchPending = true

	out, cmd := m.Update(projectsDebounceFireMsg{})
	nm := out.(Model)
	assert.False(t, nm.projectsRefetchPending, "pending flag must clear regardless")
	assert.True(t, nm.projectsStale, "stale flag preserved when fetch is skipped")
	assert.Nil(t, cmd, "inactive view → no fetch")
}

// TestProjectsDebounceFire_NoFetchWhenNotStale pins that the timer's
// wakeup is a no-op when the stale flag is unset (spurious fire after
// a manual refresh that consumed staleness). Spec §6.3.
func TestProjectsDebounceFire_NoFetchWhenNotStale(t *testing.T) {
	m := sseUpdateFixture()
	m.view = viewProjects
	m.projectsStale = false
	m.projectsRefetchPending = true

	out, cmd := m.Update(projectsDebounceFireMsg{})
	nm := out.(Model)
	assert.False(t, nm.projectsRefetchPending)
	assert.Nil(t, cmd, "stale=false → no fetch")
}

// TestProjectsLoadedMsg_ClearsStaleOnSuccessfulStatsFetch pins that a
// successful projectsLoadedMsg with non-nil stats clears
// m.projectsStale, so a subsequent debounce fire (timer that was
// already in flight before the fetch landed) doesn't trigger a
// redundant refetch. Spec §6.3.
func TestProjectsLoadedMsg_ClearsStaleOnSuccessfulStatsFetch(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewProjects
	m.projectsStale = true

	msg := projectsLoadedMsg{
		projects: map[int64]string{1: "kata"},
		idents:   map[int64]string{1: "github.com/wesm/kata"},
		stats:    map[int64]ProjectStatsSummary{1: {}},
		gen:      m.projectsGen, // captures the current gen at "dispatch" time
	}
	out, _ := m.Update(msg)
	nm := out.(Model)
	assert.False(t, nm.projectsStale, "successful stats fetch clears stale")
}

// TestProjectsLoadedMsg_DropsOlderResponse pins the race: while a
// fetchProjectsWithStats is in flight, a newer SSE invalidation can
// flip projectsStale and bump projectsGen. The older response carries
// the older gen and must be dropped entirely — neither updating the
// cache maps (which would overwrite a newer in-flight fetch's data)
// nor clearing the stale flag (which would leave the pending re-fetch
// thinking the table is fresh).
func TestProjectsLoadedMsg_DropsOlderResponse(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewProjects
	m.projectsStale = true
	m.projectsGen = 5
	// Pre-existing newer state — the older response must not overwrite.
	m.projectsByID = map[int64]string{2: "newer-data"}
	m.projectIdentByID = map[int64]string{2: "newer-ident"}
	m.projectStats = map[int64]ProjectStatsSummary{2: {Open: 99}}

	// Response carries gen=4 (an older fetch that was dispatched
	// before the latest stale-flip).
	msg := projectsLoadedMsg{
		projects: map[int64]string{1: "stale-data"},
		idents:   map[int64]string{1: "stale-ident"},
		stats:    map[int64]ProjectStatsSummary{1: {Open: 1}},
		gen:      4,
	}
	out, _ := m.Update(msg)
	nm := out.(Model)
	assert.True(t, nm.projectsStale, "older response must NOT clear stale")
	assert.Equal(t, uint64(5), nm.projectsGen, "gen unchanged on response")
	assert.Equal(t, "newer-data", nm.projectsByID[2], "older response must NOT overwrite newer data")
	_, hasStaleData := nm.projectsByID[1]
	assert.False(t, hasStaleData, "older response must NOT inject its data into the cache")
}

// TestProjectsLoadedMsg_PreservesStaleOnFailure pins that a failed
// projectsLoadedMsg (carrying err) leaves m.projectsStale armed so the
// next debounce fire retries. Spec §6.3.
func TestProjectsLoadedMsg_PreservesStaleOnFailure(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	m := initialModel(Options{})
	m.view = viewProjects
	m.toastNow = func() time.Time { return now }
	m.projectsStale = true

	msg := projectsLoadedMsg{err: errors.New("fetch failed")}
	out, _ := m.Update(msg)
	nm := out.(Model)
	assert.True(t, nm.projectsStale, "failed fetch must not clear stale")
}

// TestProjectsLoadedMsg_DropsOlderErrorResponse pins that an older-gen
// failure response is dropped without surfacing a toast. If a newer
// fetch has already landed successfully and the user is looking at
// fresh data, an older fetch's error must NOT pop a "failed to load"
// toast over the (current) UI.
func TestProjectsLoadedMsg_DropsOlderErrorResponse(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewProjects
	m.projectsGen = 5
	// Newer gen=5 response already landed — fresh data, stale cleared.
	m.projectsByID = map[int64]string{2: "fresh-data"}
	m.projectIdentByID = map[int64]string{2: "fresh-ident"}
	m.projectStats = map[int64]ProjectStatsSummary{2: {Open: 7}}
	m.projectsStale = false

	// Older fetch (gen=4) returns with an error AFTER the newer
	// success has already applied. Must NOT toast.
	msg := projectsLoadedMsg{err: errors.New("fetch failed"), gen: 4}
	out, _ := m.Update(msg)
	nm := out.(Model)
	assert.Nil(t, nm.toast, "older error response must not surface a toast")
	assert.Equal(t, "fresh-data", nm.projectsByID[2],
		"older error response must not perturb cache")
	assert.False(t, nm.projectsStale,
		"older error response must not re-arm stale")
}

// TestProjectsLoadedMsg_ClampsCursor pins that a refetch result with
// fewer rows than before still leaves m.projectsCursor pointing at a
// valid row. Without clamping, Enter on the visually-highlighted row
// silently no-ops because applyProjectsViewSelection sees cursor out
// of range.
func TestProjectsLoadedMsg_ClampsCursor(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewProjects
	m.projectsByID = map[int64]string{1: "a", 2: "b", 3: "c"}
	m.projectIdentByID = map[int64]string{1: "...", 2: "...", 3: "..."}
	m.projectStats = map[int64]ProjectStatsSummary{1: {}, 2: {}, 3: {}}
	m.projectsCursor = 3 // last row before shrink (sentinel + 3)

	msg := projectsLoadedMsg{
		projects: map[int64]string{1: "a"}, // 2 of 3 projects archived
		idents:   map[int64]string{1: "..."},
		stats:    map[int64]ProjectStatsSummary{1: {}},
	}
	out, _ := m.Update(msg)
	nm := out.(Model)
	rows := projectsRows(nm.projectsByID, nm.projectIdentByID, nm.projectStats)
	assert.Len(t, rows, 2, "sentinel + 1 project")
	assert.Less(t, nm.projectsCursor, len(rows), "cursor in range")
}

// TestHandleResetRequired_ClearsProjectsState pins that an SSE
// reset_required clears the projects-view debounce flags and dispatches
// a stats refetch when the user is in viewProjects. Without this,
// "resynced" would lie to a viewProjects user — the table numbers
// would lag the daemon. Spec §6.3 / §10 (resync semantics).
func TestHandleResetRequired_ClearsProjectsState(t *testing.T) {
	m := sseUpdateFixture()
	m.view = viewProjects
	m.projectsStale = true
	m.projectsRefetchPending = true
	m.api = &Client{}

	out, cmd := m.Update(resetRequiredMsg{})
	nm := out.(Model)
	assert.False(t, nm.projectsStale, "stale cleared")
	assert.False(t, nm.projectsRefetchPending, "pending cleared")
	require.NotNil(t, cmd, "must batch a fetch")
	// The cmd is a tea.Batch — we don't introspect it (can't reliably
	// distinguish refetch types). The flag-clearing + non-nil cmd is
	// the load-bearing assertion.
}

// keepImport keeps "fmt" referenced even when no test uses it. Removed
// when the next round of tests inevitably reintroduces a Sprintf call;
// the linter complains otherwise.
var _ = fmt.Sprintf

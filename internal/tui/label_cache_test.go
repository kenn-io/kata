package tui

import (
	"context"
	"testing"
)

// buildModelWithLabelCache returns a minimal Model with the label-
// cache state initialized. api is nil because the dispatch + acceptance
// tests inspect cache state, not Cmd execution — fetchLabelsCmd's
// nil-api branch returns a synthetic message that doesn't reach the
// network.
func buildModelWithLabelCache(_ *testing.T) Model {
	return Model{
		view:          viewList,
		projectLabels: newLabelCache(),
	}
}

// setupScopedModel returns a label-cache-initialized Model with the
// active project scope set to pid — the shared starting state for
// every dispatch/acceptance test in this file.
func setupScopedModel(t *testing.T, pid int64) Model {
	m := buildModelWithLabelCache(t)
	m.scope = scope{projectID: pid}
	return m
}

// fakeLabelLister is a labelLister stub that returns a canned slice
// without touching the network. Used by the race-fix coverage so the
// test exercises the rejection path with a real (non-empty) response,
// not the nil-api synthetic-empty fallback.
type fakeLabelLister struct {
	labels []LabelCount
	err    error
	calls  int
}

func (f *fakeLabelLister) ListLabels(_ context.Context, _ int64) ([]LabelCount, error) {
	f.calls++
	return f.labels, f.err
}

// TestLabelCache_DispatchStampsGenBeforeResponse pins the load-bearing
// invariant: dispatchLabelFetch must stamp a fresh gen on the cache
// entry BEFORE the HTTP request goes out. The race the fix prevents:
// dispatch1 (gen=1) → dispatch2 (gen=2) → cmd1 runs and returns
// labelsFetchedMsg{gen=1} → handler must drop it because the entry's
// gen has been bumped to 2.
//
// This test exercises that race end-to-end: it captures the FIRST
// cmd, fires a SECOND dispatch (which bumps cache.gen to 2), runs the
// first cmd against a fake api that returns real labels, and asserts
// the resulting gen=1 message is rejected. A regression that moved
// the gen-stamp into the cmd (or dropped the gen check in the
// handler) would let the stale labels overwrite the cache.
func TestLabelCache_DispatchStampsGenBeforeResponse(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	pid := int64(7)
	m := setupScopedModel(t, pid)
	fake := &fakeLabelLister{labels: []LabelCount{{Label: "from-first", Count: 1}}}
	// First dispatch — gen stamped to 1, fetching=true. Capture the
	// cmd; do NOT run it yet so the second dispatch can bump gen.
	m, _ = m.dispatchLabelFetch(pid)
	if got := m.projectLabels.byProject[pid].gen; got != 1 {
		t.Fatalf("first dispatch gen = %d, want 1", got)
	}
	// Build the cmd the way dispatchLabelFetch would have, but with
	// the fake lister so it returns real labels rather than nil-api
	// synthetic empties — this is what makes the rejection meaningful.
	firstCmd := fetchLabelsCmd(fake, pid, 1)
	// Second dispatch — bumps cache.gen to 2 BEFORE firstCmd runs.
	m, _ = m.dispatchLabelFetch(pid)
	if got := m.projectLabels.byProject[pid].gen; got != 2 {
		t.Fatalf("second dispatch gen = %d, want 2 (must bump even "+
			"though first cmd hasn't run)", got)
	}
	// Now run the first cmd — it produces labelsFetchedMsg{gen: 1, ...}.
	msg := firstCmd()
	nm, _ := updateModel(m, msg)
	entry := nm.projectLabels.byProject[pid]
	if len(entry.labels) != 0 {
		t.Fatalf("stale gen=1 response must be rejected because "+
			"cache.gen=2; got labels=%v", entry.labels)
	}
	if fake.calls != 1 {
		t.Fatalf("fake.ListLabels calls = %d, want 1", fake.calls)
	}
}

// TestLabelCache_StaleGenResponseDropped: a response whose gen lags
// behind the cache's current gen must NOT populate the cache. The
// acceptance check on response is gen >= cache.gen; older messages
// are silently discarded so a slow first-dispatch can't overwrite a
// freshly-invalidated cache entry.
func TestLabelCache_StaleGenResponseDropped(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	pid := int64(7)
	m := setupScopedModel(t, pid)
	m, _ = m.dispatchLabelFetch(pid) // gen=1
	m, _ = m.dispatchLabelFetch(pid) // gen=2 (newer)
	nm, _ := updateModel(m, labelsFetchedMsg{
		pid: pid, gen: 1, labels: []LabelCount{{Label: "old", Count: 1}},
	})
	entry := nm.projectLabels.byProject[pid]
	if len(entry.labels) != 0 {
		t.Fatalf("stale gen=1 response must NOT populate cache "+
			"(cache.gen=2); got labels=%v", entry.labels)
	}
}

// TestLabelCache_InactiveProjectResponseStillPopulatesCache:
// a response for an inactive project (msg.pid != targetPID()) MUST
// still populate the per-project cache entry. The cache is keyed by
// pid, so populating an inactive project's entry is harmless — and
// dropping the response (the prior contract) left entry.fetching=true
// forever, so when the user later returned to the original project
// dispatchLabelFetchIfNeeded would see "entry exists" and skip the
// re-fetch, leaving the suggestion menu stuck in "loading…" forever
// (jobs 240/241).
//
// Setup: dispatch for pid=7 (creates entry, fetching=true), switch
// scope to pid=8, then send the pid=7 response. Assert the pid=7
// entry IS populated AND fetching=false — that's the new contract.
func TestLabelCache_InactiveProjectResponseStillPopulatesCache(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	m := setupScopedModel(t, 7) // active project is 7
	// Dispatch creates the entry for pid=7 with gen=1, fetching=true.
	m, _ = m.dispatchLabelFetch(7)
	// User switches project to pid=8 BEFORE the response lands.
	m.scope = scope{projectID: 8}
	// Response for pid=7 arrives from the now-inactive fetch. The
	// per-project cache must accept it — the entry's pid matches the
	// message's pid, so the data goes in the right slot.
	nm, _ := updateModel(m, labelsFetchedMsg{
		pid: 7, gen: 1,
		labels: []LabelCount{{Label: "from7", Count: 1}},
	})
	entry := nm.projectLabels.byProject[7]
	if len(entry.labels) != 1 || entry.labels[0].Label != "from7" {
		t.Fatalf("response for pid=7 must populate the pid=7 cache "+
			"entry even when target is pid=8 (cache is per-project); "+
			"got labels=%v", entry.labels)
	}
	if entry.fetching {
		t.Fatal("entry.fetching must be false after the response is " +
			"accepted; otherwise dispatchLabelFetchIfNeeded skips a " +
			"future re-fetch and the suggestion menu stays stuck in " +
			"loading… forever (jobs 240/241)")
	}
}

// TestBatchLabelRefresh_GatesOnCacheExistence pins I1: a successful
// label.add mutation against a project the user has NEVER opened the
// `+` menu for (no cache entry yet) must NOT trigger a wasted
// ListLabels fetch. Mirrors maybeRefetchLabels's SSE gate so the two
// invalidation paths behave identically.
func TestBatchLabelRefresh_GatesOnCacheExistence(t *testing.T) {
	t.Parallel()
	m := setupScopedModel(t, 7)
	// No cache entry for pid=7 — the user never opened the menu.
	mut := mutationDoneMsg{
		kind: "label.add",
		resp: &MutationResp{Issue: &Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa"}},
	}
	out, cmd := batchLabelRefresh(m, nil, mut)
	nm := out.(Model)
	if cmd != nil {
		t.Fatal("batchLabelRefresh must not dispatch when the project " +
			"has no cache entry — wastes a ListLabels HTTP roundtrip")
	}
	if _, exists := nm.projectLabels.byProject[7]; exists {
		t.Fatal("batchLabelRefresh must not create a cache entry " +
			"for an unopened project")
	}
}

// TestBatchLabelRefresh_DispatchesWhenEntryExists: the complement of
// the gate test — when a cache entry IS present (the user has hit `+`
// against the project at least once), a successful label mutation
// MUST dispatch a refresh so the menu's count column stays accurate.
func TestBatchLabelRefresh_DispatchesWhenEntryExists(t *testing.T) {
	t.Parallel()
	m := setupScopedModel(t, 7)
	// Prime an entry as if the user had previously opened the menu.
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1,
		labels: []LabelCount{{Label: "bug", Count: 2}},
	}
	mut := mutationDoneMsg{
		kind: "label.add",
		resp: &MutationResp{Issue: &Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa"}},
	}
	out, cmd := batchLabelRefresh(m, nil, mut)
	nm := out.(Model)
	if cmd == nil {
		t.Fatal("batchLabelRefresh must dispatch when the project has " +
			"an existing cache entry")
	}
	if !nm.projectLabels.byProject[7].fetching {
		t.Fatal("dispatched entry must be fetching=true")
	}
}

// TestMutAffectsLabelCounts_AllRelevantKinds pins which mutation
// kinds invalidate the per-project label aggregate. Plan 8 commit 4
// extended the set to include "create" because the multi-field
// new-issue form can attach Labels at create time. The cache refresh
// is still gated against per-project entry existence in
// batchLabelRefresh, so a create against a project the user never
// opened the menu for remains a zero-cost no-op.
func TestMutAffectsLabelCounts_AllRelevantKinds(t *testing.T) {
	t.Parallel()
	if !mutAffectsLabelCounts(mutationDoneMsg{kind: "create"}) {
		t.Fatal("'create' must trigger a label-aggregate refetch " +
			"(commit 4 form may attach labels)")
	}
	if !mutAffectsLabelCounts(mutationDoneMsg{kind: "label.add"}) {
		t.Fatal("'label.add' must trigger a label-aggregate refetch")
	}
	if !mutAffectsLabelCounts(mutationDoneMsg{kind: "label.remove"}) {
		t.Fatal("'label.remove' must trigger a label-aggregate refetch")
	}
	// Errors short-circuit regardless of kind.
	if mutAffectsLabelCounts(mutationDoneMsg{kind: "create", err: errStub("boom")}) {
		t.Fatal("error must short-circuit refetch")
	}
}

// TestLabelCache_SSEEventInvalidatesSuggestionCacheOnly: an
// issue.labeled SSE event for a project that has a cache entry must
// trigger a refetch (entry.fetching=true, entry.gen advanced). The
// list/detail refetch path is independent — this test asserts the
// suggestion-cache invalidation specifically.
func TestLabelCache_SSEEventInvalidatesSuggestionCacheOnly(t *testing.T) { //nolint:paralleltest // applyColorMode rewrites package style vars
	pid := int64(7)
	m := setupScopedModel(t, pid)
	m.cache = newIssueCache()
	m.sseCh = nil // no SSE bridge to re-arm
	m.nextLabelsGen = 5
	m.projectLabels.byProject[pid] = labelCacheEntry{
		labels: []LabelCount{{Label: "stale", Count: 1}},
		gen:    5, pid: pid,
	}
	nm, _ := updateModel(m, eventReceivedMsg{
		eventType: "issue.labeled", projectID: pid, issueShortID: "42aa",
	})
	entry := nm.projectLabels.byProject[pid]
	if !entry.fetching {
		t.Fatal("SSE event must trigger refetch (fetching=true)")
	}
	if entry.gen <= 5 {
		t.Fatalf("gen must advance past 5; got %d", entry.gen)
	}
}

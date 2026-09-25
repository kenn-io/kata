package tui

import "testing"

// setupSeededCache returns a cache pre-populated with one issue for the
// canonical test project, plus the key used to seed it.
func setupSeededCache() (*issueCache, cacheKey) {
	c := newIssueCache()
	k := cacheKey{projectID: 7}
	c.put(k, []Issue{{ShortID: "aaa1"}})
	return c, k
}

func assertStale(t *testing.T, c *issueCache, want bool) {
	t.Helper()
	if got := c.isStale(); got != want {
		t.Fatalf("isStale = %v, want %v", got, want)
	}
}

// TestCache_PutThenStaleThenRefetch covers the steady-state SSE flow:
// fetch populates the slot, an event marks it stale, and a fresh fetch
// clears stale and replaces the data.
func TestCache_PutThenStaleThenRefetch(t *testing.T) {
	t.Parallel()
	c, k := setupSeededCache()
	assertStale(t, c, false)
	c.markStale()
	assertStale(t, c, true)
	c.put(k, []Issue{{ShortID: "bbb2"}})
	assertStale(t, c, false)
	if len(c.data) != 1 || c.data[0].ShortID != "bbb2" {
		t.Fatalf("data = %+v, want [{ShortID:bbb2}]", c.data)
	}
}

// TestCache_DropEmpties confirms drop() leaves the slot empty so a
// follow-up isStale returns false (no slot, nothing to be stale about).
// This is the sync.reset_required path.
func TestCache_DropEmpties(t *testing.T) {
	t.Parallel()
	c, _ := setupSeededCache()
	c.markStale()
	c.drop()
	assertStale(t, c, false)
	if c.set {
		t.Fatal("after drop, set must be false")
	}
	if len(c.data) != 0 {
		t.Fatalf("after drop, data must be empty, got %+v", c.data)
	}
}

// TestCache_MarkStaleIdempotent: multiple events in a 150ms window all
// flip stale; the second markStale on an already-stale cache is a no-op.
func TestCache_MarkStaleIdempotent(t *testing.T) {
	t.Parallel()
	c, _ := setupSeededCache()
	c.markStale()
	c.markStale()
	c.markStale()
	assertStale(t, c, true)
}

func TestCache_RenderFilterDoesNotChangeSlotKey(t *testing.T) {
	t.Parallel()
	m := Model{
		scope: scope{projectID: 7},
		list: listModel{filter: ListFilter{
			Status: "open", Owner: "alice", Search: "bug", Labels: []string{"prio-1"},
		}},
	}
	want := cacheKey{projectID: 7, limit: queueFetchLimit}
	if !cacheKeysEqual(m.currentCacheKey(), want) {
		t.Fatalf("currentCacheKey = %+v, want %+v", m.currentCacheKey(), want)
	}
}

// TestCache_EmptyIsNotStale: a freshly constructed cache is not stale —
// stale=true requires a real slot to be stale about.
func TestCache_EmptyIsNotStale(t *testing.T) {
	t.Parallel()
	c := newIssueCache()
	assertStale(t, c, false)
}

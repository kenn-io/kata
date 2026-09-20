package tui

// issueCache is the listModel's single-slot result cache. v1 keeps one
// snapshot of the most recently fetched list so SSE-driven refetches can
// mark stale and short-circuit redundant work — multi-key caching by
// filter is deferred until profiling shows the steady state warrants it.
//
// stale flips on every eventReceivedMsg whose payload affects the
// current view; refetched data clears it. drop() empties the slot for
// sync.reset_required so the next fetch repopulates from fresh server
// state.
type issueCache struct {
	key   cacheKey
	data  []Issue
	stale bool
	set   bool
}

// cacheKey discriminates cache slots. allProjects is folded into the
// key so a toggle from project to all-projects (Task 12's R binding)
// does not return stale single-project data.
type cacheKey struct {
	allProjects bool
	inbox       bool
	inboxVisit  uint64
	projectID   int64
	limit       int
}

func newIssueCache() *issueCache { return &issueCache{} }

// put replaces the slot with the provided key+data and clears stale.
func (c *issueCache) put(k cacheKey, data []Issue) {
	c.key = k
	c.data = data
	c.stale = false
	c.set = true
}

// markStale flips stale=true. Idempotent: a second call before the
// debounced refetch fires is a no-op so multiple events in a 150ms
// window coalesce into a single fetch.
func (c *issueCache) markStale() { c.stale = true }

// drop empties the cache. Used by sync.reset_required.
func (c *issueCache) drop() {
	c.key = cacheKey{}
	c.data = nil
	c.stale = false
	c.set = false
}

// isStale reports whether the slot holds data marked stale.
func (c *issueCache) isStale() bool { return c.set && c.stale }

package db

import "sync"

// FederationFeedCache remembers, per hub project, the newest event ID through
// which a poll found no issue.moved event stamped to the project. Without it,
// each poll of a project that never received a moved issue reads the
// project's whole event history to learn that again.
//
// A remembered ID stays true. Event inserts take IDs in commit order, so once
// a read sees an event ID, no event with a lower ID can appear later. Merging
// is refused for federated projects, and a project revision or a new replay
// horizon changes the cache key. Each poll still checks every event newer than
// the remembered ID, so the cache stays correct when other processes write to
// the same database.
type FederationFeedCache struct {
	mu      sync.Mutex
	entries map[int64]federationFeedFacts
}

type federationFeedKey struct {
	projectUID      string
	projectRevision int64
	replayHorizon   int64
}

type federationFeedFacts struct {
	key federationFeedKey
	// No issue.moved event stamped to the project has an ID at or below
	// movesThrough.
	movesThrough int64
}

// FederationFeedRead carries one read transaction's view of the cache and the
// facts it proved. A nil read uses and records nothing.
type FederationFeedRead struct {
	cache     *FederationFeedCache
	projectID int64
	known     federationFeedFacts
	learned   federationFeedFacts
	// noMovesAfter is true when this transaction found no issue.moved event
	// stamped to the project above known.movesThrough.
	noMovesAfter bool
}

// Begin returns the remembered facts for one read of project.
func (c *FederationFeedCache) Begin(project Project, binding FederationBinding) *FederationFeedRead {
	if c == nil {
		return nil
	}
	key := federationFeedKey{projectUID: project.UID, projectRevision: project.Revision, replayHorizon: binding.ReplayHorizonEventID}
	read := &FederationFeedRead{cache: c, projectID: project.ID, known: federationFeedFacts{key: key}}
	c.mu.Lock()
	defer c.mu.Unlock()
	if facts, ok := c.entries[project.ID]; ok && facts.key == key {
		read.known = facts
	}
	read.learned = read.known
	return read
}

// Commit remembers what the read proved. Call it only after the read
// transaction commits.
func (r *FederationFeedRead) Commit() {
	if r == nil || r.learned == r.known {
		return
	}
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	current, ok := r.cache.entries[r.projectID]
	if ok && current.key == r.learned.key && current.movesThrough > r.learned.movesThrough {
		// A concurrent read proved a later ID; keep it.
		return
	}
	if r.cache.entries == nil {
		r.cache.entries = map[int64]federationFeedFacts{}
	}
	r.cache.entries[r.projectID] = r.learned
}

func (r *FederationFeedRead) movesAfter() int64 {
	if r == nil {
		return 0
	}
	return r.known.movesThrough
}

func (r *FederationFeedRead) recordMoves(found bool) {
	if r != nil {
		r.noMovesAfter = !found
	}
}

// recordSnapshot notes maxID, the newest event ID in the read's snapshot.
func (r *FederationFeedRead) recordSnapshot(maxID int64) {
	if r != nil && r.noMovesAfter {
		r.learned.movesThrough = max(r.learned.movesThrough, maxID)
	}
}

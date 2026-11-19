package db

import "sync"

// FederationFeedCache remembers, per hub project, event-ID ranges already
// proven to hold no event that reaches the project's federation feed only
// because the project owns its issue. Finding such an event otherwise means
// probing every issue the project owns on each poll.
//
// A remembered range stays true. Event inserts take IDs in commit order, so
// once a read sees an event ID, no event with a lower ID can appear later; an
// issue that joins the project gets a move event stamped to the project with a
// higher ID, which every poll finds through the project's own events. Merging
// is refused for federated projects, and re-enabling federation or a project
// revision changes the cache key. Each poll still rereads the project's own
// events, its outgoing moves, and every event newer than the remembered range,
// so the cache stays correct when other processes write to the same database.
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
	// No event stamped to another project, about an issue the project owns,
	// has an ID in (foreignFrom, foreignThrough].
	foreignFrom, foreignThrough int64
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
	if ok && current.key == r.learned.key && current.foreignThrough > r.learned.foreignThrough {
		// A concurrent read proved a later range; keep it.
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

// foreignChecked returns the ID after which foreign events must be rechecked,
// when the remembered range covers everything from afterID on.
func (r *FederationFeedRead) foreignChecked(afterID int64) (int64, bool) {
	if r == nil || r.known.foreignThrough == 0 || r.known.foreignFrom > afterID {
		return 0, false
	}
	return max(afterID, r.known.foreignThrough), true
}

// recordQuiet notes that no feed event has an ID in (afterID, maxID].
func (r *FederationFeedRead) recordQuiet(afterID, maxID int64) {
	if r == nil || maxID <= afterID {
		return
	}
	from := afterID
	if r.known.foreignThrough != 0 && r.known.foreignFrom <= afterID && afterID <= r.known.foreignThrough {
		from = r.known.foreignFrom
	}
	r.learned.foreignFrom, r.learned.foreignThrough = from, maxID
	if r.noMovesAfter {
		r.learned.movesThrough = maxID
	}
}

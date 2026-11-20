package db

import "sync"

// FederationFeedCache remembers, per hub project, event-ID ranges already
// proven to hold no event that reaches the project's federation feed only
// because the project owns its issue. Finding such an event otherwise means
// probing every issue the project owns on each poll. It also remembers the
// newest event ID through which no moved issue entered the project, so a poll
// need not reread the project's move history.
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
	// issues is how many issues the project owned when a read last counted
	// them, if issuesKnown. It only sizes page windows: a stale count changes
	// what a poll costs, never what it returns.
	issues      int64
	issuesKnown bool
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
	// counted is true when this transaction counted the project's issues.
	counted bool
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
	learned := r.learned
	if current, ok := r.cache.entries[r.projectID]; ok && current.key == learned.key {
		// Keep the newest proof of each kind a concurrent read committed.
		learned.movesThrough = max(learned.movesThrough, current.movesThrough)
		if current.foreignThrough > learned.foreignThrough {
			learned.foreignFrom, learned.foreignThrough = current.foreignFrom, current.foreignThrough
		}
	}
	if r.cache.entries == nil {
		r.cache.entries = map[int64]federationFeedFacts{}
	}
	r.cache.entries[r.projectID] = learned
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

// knownIssues returns the remembered issue count, when there is one.
func (r *FederationFeedRead) knownIssues() (int64, bool) {
	if r == nil || !r.known.issuesKnown {
		return 0, false
	}
	return r.known.issues, true
}

// recordIssues notes how many issues the project owns.
func (r *FederationFeedRead) recordIssues(issues int64) {
	if r != nil {
		r.learned.issues, r.learned.issuesKnown, r.counted = issues, true, true
	}
}

// recordEvents notes that the read returned events. Every change to which
// issues the project owns reaches its feed as an event, and a move out of the
// project is stamped to the destination, so a count this read reused may be
// stale; the next read counts again.
func (r *FederationFeedRead) recordEvents() {
	if r != nil && !r.counted {
		r.learned.issues, r.learned.issuesKnown = 0, false
	}
}

// recordSnapshot notes maxID, the newest event ID in the read's snapshot. The
// move check covers the whole snapshot, whatever the read returned.
func (r *FederationFeedRead) recordSnapshot(maxID int64) {
	if r != nil && r.noMovesAfter {
		r.learned.movesThrough = max(r.learned.movesThrough, maxID)
	}
}

// recordQuiet notes that no feed event has an ID in (afterID, maxID].
func (r *FederationFeedRead) recordQuiet(afterID, maxID int64) {
	if r == nil || maxID <= afterID {
		return
	}
	from := afterID
	if r.known.foreignFrom <= afterID && afterID <= r.known.foreignThrough {
		from = r.known.foreignFrom
	}
	r.learned.foreignFrom, r.learned.foreignThrough = from, maxID
}

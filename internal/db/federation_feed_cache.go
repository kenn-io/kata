package db

import "sync"

// FederationFeedCache remembers, per hub project, event-ID ranges already
// proven to hold no event that reaches the project's federation feed only
// because the project owns its issue. Finding such an event otherwise means
// probing every issue the project owns on each poll. It also remembers what
// the project's move history requires of a peer, so a poll need not reread
// that history.
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
	// movesThrough. Deleting events cannot make this false.
	movesThrough int64
	// When featuresThrough is set, moved and nativeMoved held when the newest
	// event ID was featuresThrough: whether an issue.moved event is stamped to
	// the project, and whether an issue it moved in still carries cron
	// delivery state. A change to either adds an event stamped to the project,
	// moves an issue out of it, or purges one of its issues with a reset
	// cursor above every event it saw, so they hold until one of those
	// follows featuresThrough.
	featuresThrough    int64
	moved, nativeMoved bool
	// issues is how many issues the project owned in the snapshot whose
	// newest event ID is issuesAt, if issuesKnown. When issuesKnown is false,
	// issuesAt is the snapshot in which a read found the count stale. The
	// count only sizes page windows: a stale count changes what a poll costs,
	// never what it returns.
	issues      int64
	issuesKnown bool
	issuesAt    int64
}

// FederationFeedRead carries one read transaction's view of the cache and the
// facts it proved. A nil read uses and records nothing.
type FederationFeedRead struct {
	cache     *FederationFeedCache
	projectID int64
	postgres  bool
	known     federationFeedFacts
	learned   federationFeedFacts
	// The move checks this read ran, remembered once the read learns its
	// snapshot's newest event ID.
	movesChecked, moved, nativeChecked, nativeMoved bool
	// snapshot is the newest event ID in this transaction's snapshot.
	snapshot int64
}

// Begin returns the remembered facts for one read of project on a PostgreSQL
// or SQLite store.
func (c *FederationFeedCache) Begin(project Project, binding FederationBinding, postgres bool) *FederationFeedRead {
	if c == nil {
		return nil
	}
	key := federationFeedKey{projectUID: project.UID, projectRevision: project.Revision, replayHorizon: binding.ReplayHorizonEventID}
	read := &FederationFeedRead{cache: c, projectID: project.ID, postgres: postgres, known: federationFeedFacts{key: key}}
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
		if current.featuresThrough > learned.featuresThrough {
			learned.featuresThrough, learned.moved, learned.nativeMoved = current.featuresThrough, current.moved, current.nativeMoved
		}
		// A count or recount mark from a newer snapshot wins; at the same
		// snapshot, a count wins over a mark.
		if current.issuesAt > learned.issuesAt || (current.issuesAt == learned.issuesAt && current.issuesKnown) {
			learned.issues, learned.issuesKnown, learned.issuesAt = current.issues, current.issuesKnown, current.issuesAt
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

// moveFeatures returns the remembered move facts and the event ID they held
// at, if any.
func (r *FederationFeedRead) moveFeatures() (through int64, moved, nativeMoved bool) {
	if r == nil {
		return 0, false, false
	}
	return r.known.featuresThrough, r.known.moved, r.known.nativeMoved
}

// recordMoves notes whether an issue.moved event is stamped to the project.
func (r *FederationFeedRead) recordMoves(found bool) {
	if r != nil {
		r.movesChecked, r.moved = true, found
	}
}

// recordNativeMoves notes whether an issue the project moved in still
// carries cron delivery state.
func (r *FederationFeedRead) recordNativeMoves(native bool) {
	if r != nil {
		r.nativeChecked, r.nativeMoved = true, native
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

// recordIssues notes how many issues the project owns in this snapshot. Call
// it after recordSnapshot.
func (r *FederationFeedRead) recordIssues(issues int64) {
	if r != nil {
		r.learned.issues, r.learned.issuesKnown, r.learned.issuesAt = issues, true, r.snapshot
	}
}

// recordEvents notes that the read returned events. Every change to which
// issues the project owns reaches its feed as an event, and a move out of the
// project is stamped to the destination, so a count from an older snapshot
// may be stale; the next read counts again.
func (r *FederationFeedRead) recordEvents() {
	if r != nil && r.learned.issuesAt < r.snapshot {
		r.learned.issues, r.learned.issuesKnown, r.learned.issuesAt = 0, false, r.snapshot
	}
}

// recordSnapshot notes maxID, the newest event ID in the read's snapshot. The
// move checks cover the whole snapshot, whatever the read returned.
func (r *FederationFeedRead) recordSnapshot(maxID int64) {
	if r == nil {
		return
	}
	r.snapshot = maxID
	switch {
	case !r.movesChecked:
	case !r.moved:
		r.learned.movesThrough = max(r.learned.movesThrough, maxID)
		r.learned.featuresThrough, r.learned.moved, r.learned.nativeMoved = maxID, false, false
	case r.nativeChecked:
		r.learned.featuresThrough, r.learned.moved, r.learned.nativeMoved = maxID, true, r.nativeMoved
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

package pgstore

import (
	"context"
	"database/sql"
	"sync"

	"go.kenn.io/kata/internal/db"
)

type relayHistoryValidationKey struct {
	projectID  int64
	bindingUID string
}

type relayHistoryValidationEntry struct {
	projectUID   string
	generation   uint64
	creations    uint64
	shortParents bool
	after        int64
	cursor       int64
	reset        bool
}

type relayHistoryValidationCache struct {
	mu                 sync.Mutex
	initialized        bool
	generation         uint64
	creations          uint64
	eventCursor        int64
	purgeCursor        int64
	projectPurgeCursor int64
	entries            map[relayHistoryValidationKey]relayHistoryValidationEntry
}

func newRelayHistoryValidationCache() *relayHistoryValidationCache {
	return &relayHistoryValidationCache{entries: make(map[relayHistoryValidationKey]relayHistoryValidationEntry)}
}

func (cache *relayHistoryValidationCache) invalidate() {
	cache.mu.Lock()
	cache.generation++
	cache.mu.Unlock()
}

func (s *Store) invalidateRelayHistoryValidationCache() {
	if s.relayHistoryCache != nil {
		s.relayHistoryCache.invalidate()
	}
}

// relayHistoryNeedsResetCachedTx validates unchanged history once, then checks
// only new source events. Retained checkpoints have independent grant/boundary
// entries. Moves, merges, renames, and purges invalidate results when retained
// references can resolve differently. Creations and unrelated snapshots
// revalidate only short-parent references or reset results.
func (s *Store) relayHistoryNeedsResetCachedTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	projectUID string,
	checkpoint *db.RelayResetCheckpoint,
	scan bool,
) (bool, error) {
	key := relayHistoryValidationKey{projectID: projectID}
	var after int64
	validateAfter := relayHistoryHasBoundaryAfterTx
	if checkpoint != nil {
		// A root checkpoint already introduces existing issues, even when
		// their raw history was compacted. Validate only later privacy
		// crossings; the caller checks root purge boundaries.
		validateAfter = relayHistoryCrossesProjectAfterTx
		key.bindingUID = checkpoint.Translation.Authority.BindingUID
		after = checkpoint.Manifest.HistoryEventID
	}
	// mu covers only bookkeeping, taken before the fill's first read so
	// lock order equals snapshot order. Scans run unlocked; entries carry the
	// stamps this snapshot observed, so a late publication fails reuse.
	cache := s.relayHistoryCache
	cache.mu.Lock()

	var eventCursor, purgeCursor, projectPurgeCursor int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(id) FROM events),0),
		COALESCE((SELECT MAX(id) FROM purge_log),0),
		COALESCE((SELECT MAX(id) FROM project_purge_log),0)`).Scan(
		&eventCursor, &purgeCursor, &projectPurgeCursor,
	); err != nil {
		cache.mu.Unlock()
		return false, mapSQLError(err, nil)
	}
	if !cache.initialized {
		cache.initialized = true
		cache.generation = 1
	} else {
		invalidated := purgeCursor != cache.purgeCursor || projectPurgeCursor != cache.projectPurgeCursor || eventCursor < cache.eventCursor
		var created bool
		if !invalidated && eventCursor > cache.eventCursor {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events
				WHERE id>$1 AND type IN ('issue.moved','project.merged','project.renamed','project.federation_enabled')),
				EXISTS(SELECT 1 FROM events WHERE id>$1 AND type IN ('issue.created','issue.snapshot'))`,
				cache.eventCursor,
			).Scan(&invalidated, &created); err != nil {
				cache.mu.Unlock()
				return false, mapSQLError(err, nil)
			}
		}
		if invalidated {
			cache.generation++
		} else if created {
			cache.creations++
		}
	}
	cache.eventCursor = eventCursor
	cache.purgeCursor = purgeCursor
	cache.projectPurgeCursor = projectPurgeCursor

	var projectCursor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=$1`, projectID).Scan(&projectCursor); err != nil {
		cache.mu.Unlock()
		return false, mapSQLError(err, nil)
	}
	entry, cached := cache.entries[key]
	generation, creations := cache.generation, cache.creations
	cache.mu.Unlock()

	// Adoption and relay-reset install write snapshot-only history into the
	// project they replace.
	var snapshot bool
	if cached && projectCursor > entry.cursor {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE project_id=$1 AND id>$2 AND type='issue.snapshot')`, projectID, entry.cursor).Scan(&snapshot); err != nil {
			return false, mapSQLError(err, nil)
		}
	}
	full := snapshot || !cached || entry.projectUID != projectUID || entry.after != after || entry.generation != generation || projectCursor < entry.cursor || entry.creations != creations && (entry.reset || entry.shortParents)
	if !full && projectCursor == entry.cursor {
		return entry.reset, nil
	}
	if !scan {
		return false, &relayHistoryCacheMiss{projectID: projectID, projectUID: projectUID, checkpoint: checkpoint}
	}
	from := entry.cursor
	if full {
		if s.relayHistoryFullScanObserver != nil {
			s.relayHistoryFullScanObserver()
		}
		entry, from = relayHistoryValidationEntry{projectUID: projectUID, after: after}, after
	}
	var reset bool
	var err error
	if full && checkpoint == nil {
		reset, err = relayHistoryNeedsResetTx(ctx, tx, projectID, projectUID)
	} else {
		reset, err = validateAfter(ctx, tx, projectID, from)
	}
	if err != nil {
		return false, err
	}
	shortParents, err := relayHistoryResolvesShortParentAfterTx(ctx, tx, projectID, from)
	if err != nil {
		return false, err
	}
	entry.generation, entry.creations, entry.cursor = generation, creations, projectCursor
	entry.shortParents = entry.shortParents || shortParents
	entry.reset = entry.reset || reset
	cache.mu.Lock()
	cache.entries[key] = entry
	cache.mu.Unlock()
	return entry.reset, nil
}

// relayHistoryResolvesShortParentAfterTx detects the only references whose
// project scope can become ambiguous after an issue is created elsewhere.
// Bare evidence references use the event's project; qualified names can change
// on rename, which invalidates all entries above. UID references match exactly.
func relayHistoryResolvesShortParentAfterTx(ctx context.Context, tx *sql.Tx, projectID, after int64) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE project_id=$1 AND id>$2 AND type='issue.closed' AND COALESCE(payload::jsonb->>'parent_uid','')='' AND COALESCE(payload::jsonb->>'parent_short_id','')<>'')`, projectID, after).Scan(&found)
	return found, mapSQLError(err, nil)
}

// relayHistoryCacheMiss asks the caller to fill this exact key outside the
// fenced transaction, after its authority checks have succeeded.
type relayHistoryCacheMiss struct {
	projectID  int64
	projectUID string
	checkpoint *db.RelayResetCheckpoint
}

func (*relayHistoryCacheMiss) Error() string { return "relay history validation needs a fill" }

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
	projectUID string
	generation uint64
	after      int64
	cursor     int64
	reset      bool
}

type relayHistoryValidationCache struct {
	mu                 sync.Mutex
	initialized        bool
	generation         uint64
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
// entries. Issue identity changes and purges invalidate cached
// results because they can change how retained event references resolve.
func (s *Store) relayHistoryNeedsResetCachedTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	projectUID string,
	checkpoint *db.RelayResetCheckpoint,
) (bool, error) {
	key := relayHistoryValidationKey{projectID: projectID}
	var after int64
	validateAfter := relayHistoryHasBoundaryAfterTx
	if checkpoint != nil {
		// A root checkpoint already introduces existing issues, even when
		// their raw history was compacted. Only later privacy crossings need
		// a new checkpoint here; the caller checks root purge boundaries.
		validateAfter = relayHistoryCrossesProjectAfterTx
		key.bindingUID = checkpoint.Translation.Authority.BindingUID
		after = checkpoint.Manifest.HistoryEventID
	}
	cache := s.relayHistoryCache
	cache.mu.Lock()
	defer cache.mu.Unlock()

	var eventCursor, purgeCursor, projectPurgeCursor int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(id) FROM events),0),
		COALESCE((SELECT MAX(id) FROM purge_log),0),
		COALESCE((SELECT MAX(id) FROM project_purge_log),0)`).Scan(
		&eventCursor, &purgeCursor, &projectPurgeCursor,
	); err != nil {
		return false, mapSQLError(err, nil)
	}
	if !cache.initialized {
		cache.initialized = true
		cache.generation = 1
	} else {
		invalidated := purgeCursor != cache.purgeCursor || projectPurgeCursor != cache.projectPurgeCursor || eventCursor < cache.eventCursor
		if !invalidated && eventCursor > cache.eventCursor {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events
				WHERE id>$1 AND type IN ('issue.created','issue.snapshot','issue.moved','project.merged','project.federation_enabled'))`,
				cache.eventCursor,
			).Scan(&invalidated); err != nil {
				return false, mapSQLError(err, nil)
			}
		}
		if invalidated {
			cache.generation++
		}
	}
	cache.eventCursor = eventCursor
	cache.purgeCursor = purgeCursor
	cache.projectPurgeCursor = projectPurgeCursor

	var projectCursor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=$1`, projectID).Scan(&projectCursor); err != nil {
		return false, mapSQLError(err, nil)
	}
	entry, cached := cache.entries[key]
	if !cached || entry.projectUID != projectUID || entry.after != after || entry.generation != cache.generation || projectCursor < entry.cursor {
		if s.relayHistoryFullScanObserver != nil {
			s.relayHistoryFullScanObserver()
		}
		var reset bool
		var err error
		if checkpoint == nil {
			reset, err = relayHistoryNeedsResetTx(ctx, tx, projectID, projectUID)
		} else {
			reset, err = validateAfter(ctx, tx, projectID, after)
		}
		if err != nil {
			return false, err
		}
		cache.entries[key] = relayHistoryValidationEntry{
			projectUID: projectUID,
			generation: cache.generation,
			after:      after,
			cursor:     projectCursor,
			reset:      reset,
		}
		return reset, nil
	}
	if projectCursor == entry.cursor {
		return entry.reset, nil
	}
	reset, err := validateAfter(ctx, tx, projectID, entry.cursor)
	if err != nil {
		return false, err
	}
	entry.cursor = projectCursor
	entry.reset = entry.reset || reset
	cache.entries[key] = entry
	return entry.reset, nil
}

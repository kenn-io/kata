package sqlitestore

import (
	"context"
	"database/sql"
	"sync"
)

type relayHistoryValidationEntry struct {
	projectUID string
	generation uint64
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
	entries            map[int64]relayHistoryValidationEntry
}

func newRelayHistoryValidationCache() *relayHistoryValidationCache {
	return &relayHistoryValidationCache{entries: make(map[int64]relayHistoryValidationEntry)}
}

func (cache *relayHistoryValidationCache) invalidate() {
	cache.mu.Lock()
	cache.generation++
	cache.mu.Unlock()
}

func (d *Store) invalidateRelayHistoryValidationCache() {
	if d.relayHistoryCache != nil {
		d.relayHistoryCache.invalidate()
	}
}

// relayHistoryNeedsResetCachedTx validates unchanged history once, then checks
// only new source events. Issue identity changes and purges invalidate cached
// results because they can change how retained event references resolve.
func (d *Store) relayHistoryNeedsResetCachedTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	projectUID string,
) (bool, error) {
	cache := d.relayHistoryCache
	cache.mu.Lock()
	defer cache.mu.Unlock()

	var eventCursor, purgeCursor, projectPurgeCursor int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(id) FROM events),0),
		COALESCE((SELECT MAX(id) FROM purge_log),0),
		COALESCE((SELECT MAX(id) FROM project_purge_log),0)`).Scan(
		&eventCursor, &purgeCursor, &projectPurgeCursor,
	); err != nil {
		return false, err
	}
	if !cache.initialized {
		cache.initialized = true
		cache.generation = 1
	} else {
		invalidated := purgeCursor != cache.purgeCursor || projectPurgeCursor != cache.projectPurgeCursor || eventCursor < cache.eventCursor
		if !invalidated && eventCursor > cache.eventCursor {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events
				WHERE id>? AND type IN ('issue.created','issue.snapshot','issue.moved','project.merged','project.federation_enabled'))`,
				cache.eventCursor,
			).Scan(&invalidated); err != nil {
				return false, err
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
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE project_id=?`, projectID).Scan(&projectCursor); err != nil {
		return false, err
	}
	entry, cached := cache.entries[projectID]
	if !cached || entry.projectUID != projectUID || entry.generation != cache.generation || projectCursor < entry.cursor {
		if d.relayHistoryFullScanObserver != nil {
			d.relayHistoryFullScanObserver()
		}
		reset, err := relayHistoryNeedsResetTx(ctx, tx, projectID, projectUID)
		if err != nil {
			return false, err
		}
		cache.entries[projectID] = relayHistoryValidationEntry{
			projectUID: projectUID,
			generation: cache.generation,
			cursor:     projectCursor,
			reset:      reset,
		}
		return reset, nil
	}
	if projectCursor == entry.cursor {
		return entry.reset, nil
	}
	reset, err := relayHistoryHasBoundaryAfterTx(ctx, tx, projectID, entry.cursor)
	if err != nil {
		return false, err
	}
	entry.cursor = projectCursor
	entry.reset = entry.reset || reset
	cache.entries[projectID] = entry
	return entry.reset, nil
}

package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
)

// RetainTrackerMetadataSQL snapshots target evidence before full replacement.
// Keeping it inactive prevents a source backup from erasing a newer dispatched
// effect. These are local fences, not restored credentials or activation grants.
func RetainTrackerMetadataSQL(ctx context.Context, tx *sql.Tx) ([]MetaKV, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM meta WHERE substr(key,1,$1)=$2`, len(TrackerMetadataPrefix), TrackerMetadataPrefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var retained []MetaKV
	for rows.Next() {
		var rec MetaKV
		if err := rows.Scan(&rec.Key, &rec.Value); err != nil {
			return nil, err
		}
		normalized, keep, err := TrackerMetadataForImport(rec, ImportOptions{})
		if err != nil {
			return nil, err
		}
		if keep {
			retained = append(retained, normalized)
		}
	}
	return retained, rows.Err()
}

// RestoreRetainedTrackerMetadataSQL preserves target evidence during replacement.
func RestoreRetainedTrackerMetadataSQL(ctx context.Context, tx *sql.Tx, records []MetaKV) error {
	// Imported and retained pending receipts are made inactive; rebuild local counts.
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, TrackerControlCapacityKey); err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, rec.Key, rec.Value); err != nil {
			return err
		}
	}
	return nil
}

// CheckTrackerMetadataRestoreConflict refuses to destroy retained newer or
// different evidence. Restore onto a fresh target when histories diverge; both
// snapshots still require the separate runtime activation/reconciliation barrier.
func CheckTrackerMetadataRestoreConflict(previous string, next MetaKV) error {
	old, keep, err := TrackerMetadataForImport(MetaKV{Key: next.Key, Value: previous}, ImportOptions{})
	if err != nil || !keep {
		return ErrTrackerFenceInvalid
	}
	var left, right any
	if json.Unmarshal([]byte(old.Value), &left) != nil || json.Unmarshal([]byte(next.Value), &right) != nil {
		return ErrTrackerFenceInvalid
	}
	l, err := json.Marshal(left, json.Deterministic(true))
	if err != nil {
		return ErrTrackerFenceInvalid
	}
	r, err := json.Marshal(right, json.Deterministic(true))
	if err != nil {
		return ErrTrackerFenceInvalid
	}
	if string(l) != string(r) {
		return ErrTrackerOutcomeUnknown
	}
	return nil
}

// ValidateTrackerMetadataRestoreSQL checks a whole import against retained target
// evidence. One lane/sequence index replaces per-record scans of growing lanes.
// Call it inside the replay transaction, after retaining target metadata and
// before writing incoming records, in the same order those records will replay.
func ValidateTrackerMetadataRestoreSQL(ctx context.Context, tx *sql.Tx, records []ImportRecord, opts ImportOptions) error {
	if opts.MergeProject || opts.NewInstance {
		return nil
	}
	retained, err := RetainTrackerMetadataSQL(ctx, tx)
	if err != nil {
		return err
	}
	index := trackerRestoreIndex{
		byKey: make(map[string]MetaKV),
		lanes: make(map[string]map[uint64]map[string]TrackerExecutionFence),
	}
	for _, record := range retained {
		index.add(record)
	}
	for _, record := range records {
		meta, ok := record.(*MetaKV)
		if !ok || !strings.HasPrefix(meta.Key, TrackerMetadataPrefix) {
			continue
		}
		normalized, keep, err := TrackerMetadataForImport(*meta, opts)
		if err != nil {
			return err
		}
		if !keep {
			continue
		}
		if err := index.check(normalized); err != nil {
			return err
		}
		index.add(normalized)
	}
	return nil
}

type trackerRestoreIndex struct {
	byKey map[string]MetaKV
	lanes map[string]map[uint64]map[string]TrackerExecutionFence
}

func trackerRestoreFence(record MetaKV) (TrackerExecutionFence, string, bool) {
	var fence TrackerExecutionFence
	if json.Unmarshal([]byte(record.Value), &fence, json.RejectUnknownMembers(true)) != nil || fence.Validate() != nil || !trackerImportKeyMatches(record.Key, fence) {
		return fence, "", false // Other tracker metadata has its own strict validator.
	}
	base, _ := TrackerExecutionFenceKey(fence.RootInstanceUID, fence.ProjectUID, fence.Role)
	return fence, base, true
}

func (index *trackerRestoreIndex) add(record MetaKV) {
	// A head key can be replaced; keep only its current sequence in the lookup.
	if previous, ok := index.byKey[record.Key]; ok {
		if fence, base, ok := trackerRestoreFence(previous); ok {
			delete(index.lanes[base][fence.Sequence], record.Key)
		}
	}
	index.byKey[record.Key] = record
	fence, base, ok := trackerRestoreFence(record)
	if !ok {
		return
	}
	if index.lanes[base] == nil {
		index.lanes[base] = make(map[uint64]map[string]TrackerExecutionFence)
	}
	if index.lanes[base][fence.Sequence] == nil {
		index.lanes[base][fence.Sequence] = make(map[string]TrackerExecutionFence)
	}
	index.lanes[base][fence.Sequence][record.Key] = fence
}

func (index *trackerRestoreIndex) check(record MetaKV) error {
	if previous, ok := index.byKey[record.Key]; ok {
		if err := CheckTrackerMetadataRestoreConflict(previous.Value, record); err != nil {
			return err
		}
	}
	incoming, base, ok := trackerRestoreFence(record)
	if !ok {
		return nil
	}
	lane := index.lanes[base]
	for _, sequence := range []uint64{incoming.Sequence - 1, incoming.Sequence, incoming.Sequence + 1} {
		for key, existing := range lane[sequence] {
			if key == record.Key {
				continue
			}
			if existing.Sequence == incoming.Sequence {
				// Activation can refresh only a settled local head's process epoch;
				// its terminal history intentionally retains the original epoch.
				if (key == base || record.Key == base) && existing.Remote == nil && incoming.Remote == nil && trackerFenceSettled(existing) && existing.Phase == incoming.Phase {
					existing.ProcessEpoch = incoming.ProcessEpoch
				}
				left, err := json.Marshal(existing, json.Deterministic(true))
				if err != nil {
					return err
				}
				right, err := json.Marshal(incoming, json.Deterministic(true))
				if err != nil {
					return err
				}
				if string(left) != string(right) {
					return ErrTrackerOutcomeUnknown
				}
			} else if incoming.Sequence == existing.Sequence+1 && incoming.PreviousHead != TrackerFenceHead(existing) {
				return ErrTrackerOutcomeUnknown
			} else if existing.Sequence == incoming.Sequence+1 && existing.PreviousHead != TrackerFenceHead(incoming) {
				return ErrTrackerOutcomeUnknown
			}
		}
	}
	return nil
}

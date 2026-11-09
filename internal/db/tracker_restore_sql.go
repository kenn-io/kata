package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
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
		normalized, _, err := TrackerMetadataForImport(rec, ImportOptions{})
		if err != nil {
			return nil, err
		}
		retained = append(retained, normalized)
	}
	return retained, rows.Err()
}

// RestoreRetainedTrackerMetadataSQL preserves target evidence during replacement.
func RestoreRetainedTrackerMetadataSQL(ctx context.Context, tx *sql.Tx, records []MetaKV) error {
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

// CheckTrackerMetadataRestoreSQL rejects conflicts with retained tracker evidence.
func CheckTrackerMetadataRestoreSQL(ctx context.Context, tx *sql.Tx, record MetaKV, postgres bool) error {
	if !strings.HasPrefix(record.Key, TrackerMetadataPrefix) {
		return nil
	}
	var previous string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, record.Key).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err := CheckTrackerMetadataRestoreConflict(previous, record); err != nil {
			return err
		}
	}
	// A head, terminal history and archived non-send occupy different keys.
	// A restore cannot combine incompatible facts for the same lane/sequence.
	var incoming TrackerExecutionFence
	if json.Unmarshal([]byte(record.Value), &incoming, json.RejectUnknownMembers(true)) != nil || incoming.Validate() != nil || !trackerImportKeyMatches(record.Key, incoming) {
		return nil // Other tracker metadata has its own strict import validator.
	}
	base, err := TrackerExecutionFenceKey(incoming.RootInstanceUID, incoming.ProjectUID, incoming.Role)
	if err != nil {
		return err
	}
	order := "key COLLATE BINARY"
	if postgres {
		order = `key COLLATE "C"`
	}
	// The canonical lane consists of its head and keys under base+'.'. The
	// exclusive upper bound '/' is the next byte after '.', on both backends.
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM meta WHERE key=$1 OR (`+order+` >= $2 AND `+order+` < $3)`, base, base+".", base+"/")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return err
		}
		if key == record.Key {
			continue
		}
		normalized, keep, err := TrackerMetadataForImport(MetaKV{Key: key, Value: value}, ImportOptions{})
		if err != nil || !keep {
			return ErrTrackerFenceInvalid
		}
		var existing TrackerExecutionFence
		if err := json.Unmarshal([]byte(normalized.Value), &existing, json.RejectUnknownMembers(true)); err != nil || existing.Validate() != nil || !trackerImportKeyMatches(key, existing) {
			return ErrTrackerFenceInvalid
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
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

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

func CheckTrackerMetadataRestoreSQL(ctx context.Context, tx *sql.Tx, record MetaKV) error {
	if !strings.HasPrefix(record.Key, TrackerMetadataPrefix) {
		return nil
	}
	var previous string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, record.Key).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return CheckTrackerMetadataRestoreConflict(previous, record)
}

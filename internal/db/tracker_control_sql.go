package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

// TrackerControlStore retains idempotent named control receipts.
type TrackerControlStore interface {
	BeginTrackerControl(context.Context, tracker.ControlReceipt) (tracker.ControlReceipt, bool, error)
	CompleteTrackerControl(context.Context, tracker.ControlReceipt, tracker.ControlReceipt) error
	ReadTrackerControl(context.Context, string, string) (tracker.ControlReceipt, error)
}

// TrackerControlKey returns the local metadata key for a control receipt.
func TrackerControlKey(root, project, operation string) string {
	return TrackerMetadataPrefix + "control." + project + "." + root + "." + operation
}
func (a TrackerFenceSQL) controlLocal(c tracker.ControlReceipt) bool {
	return c.RootInstanceUID == a.InstanceUID && c.Validate() == nil
}
func decodeTrackerControl(key, raw string) (tracker.ControlReceipt, error) {
	var c tracker.ControlReceipt
	if len(raw) > 16<<10 || json.Unmarshal([]byte(raw), &c, json.RejectUnknownMembers(true)) != nil || c.Validate() != nil || key != TrackerControlKey(c.RootInstanceUID, c.ProjectUID, c.UID) {
		return tracker.ControlReceipt{}, tracker.ErrInvalid
	}
	return c, nil
}

// ReadControl reads an authorized project control receipt.
func (a TrackerFenceSQL) ReadControl(ctx context.Context, project, operation string) (tracker.ControlReceipt, error) {
	if !uid.Valid(project) || !uid.Valid(operation) {
		return tracker.ControlReceipt{}, tracker.ErrInvalid
	}
	var result tracker.ControlReceipt
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		result = tracker.ControlReceipt{}
		if err := a.authorize(ctx, tx, project); err != nil {
			return err
		}
		key := TrackerControlKey(a.InstanceUID, project, operation)
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var err error
		result, err = decodeTrackerControl(key, raw)
		return err
	})
	return result, err
}

// BeginControl retains a pending receipt or returns its existing result.
func (a TrackerFenceSQL) BeginControl(ctx context.Context, p tracker.ControlReceipt) (tracker.ControlReceipt, bool, error) {
	if !a.controlLocal(p) || p.State != tracker.ControlPending {
		return tracker.ControlReceipt{}, false, tracker.ErrInvalid
	}
	var result tracker.ControlReceipt
	fresh := false
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		result = tracker.ControlReceipt{}
		fresh = false
		if err := a.authorize(ctx, tx, p.ProjectUID); err != nil {
			return err
		}
		key := TrackerControlKey(a.InstanceUID, p.ProjectUID, p.UID)
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
		if err == nil {
			existing, err := decodeTrackerControl(key, raw)
			if err != nil {
				return err
			}
			if !tracker.SameControlRequest(p, existing) {
				return tracker.ErrControlConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := trackerControlCapacity(ctx, tx, p); err != nil {
			return err
		}
		encoded, err := json.Marshal(p)
		if err != nil {
			return tracker.ErrInvalid
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, key, string(encoded))
		if err != nil {
			return err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if count == 1 {
			result = p
			fresh = true
			return nil
		}
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw); err != nil {
			return err
		}
		existing, err := decodeTrackerControl(key, raw)
		if err != nil {
			return err
		}
		if !tracker.SameControlRequest(p, existing) {
			return tracker.ErrControlConflict
		}
		result = existing
		return nil
	})
	return result, fresh, err
}
func trackerControlCapacity(ctx context.Context, tx *sql.Tx, p tracker.ControlReceipt) error {
	prefix := TrackerMetadataPrefix + "control."
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM meta WHERE substr(key,1,$1)=$2`, len(prefix), prefix)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	pending, project, connection := 0, 0, 0
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		c, err := decodeTrackerControl(key, raw)
		if err != nil {
			return err
		}
		if c.State != tracker.ControlPending {
			continue
		}
		pending++
		if c.ProjectUID == p.ProjectUID {
			project++
		}
		if c.Selection.OwnerInstanceUID == p.Selection.OwnerInstanceUID && c.Selection.ConnectionUID == p.Selection.ConnectionUID {
			connection++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if pending >= 256 || project >= 64 || connection >= 64 {
		return tracker.ErrControlCapacity
	}
	return nil
}

// CompleteControl atomically replaces a matching pending receipt.
func (a TrackerFenceSQL) CompleteControl(ctx context.Context, previous, next tracker.ControlReceipt) error {
	if !a.controlLocal(previous) || !a.controlLocal(next) || previous.State != tracker.ControlPending || previous.ProcessEpoch != next.ProcessEpoch || (next.State != tracker.ControlComplete && next.State != tracker.ControlFailed) || !tracker.SameControlRequest(previous, next) {
		return tracker.ErrInvalid
	}
	before, err := json.Marshal(previous)
	if err != nil {
		return tracker.ErrInvalid
	}
	after, err := json.Marshal(next)
	if err != nil {
		return tracker.ErrInvalid
	}
	return a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, next.ProjectUID); err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`, string(after), TrackerControlKey(next.RootInstanceUID, next.ProjectUID, next.UID), string(before))
		if err != nil {
			return err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return tracker.ErrControlConflict
		}
		return nil
	})
}
func isTrackerControlKey(key string) bool {
	return strings.HasPrefix(key, TrackerMetadataPrefix+"control.")
}
func trackerControlMetadataForImport(record MetaKV) (MetaKV, error) {
	c, err := decodeTrackerControl(record.Key, record.Value)
	if err != nil {
		return MetaKV{}, err
	}
	if c.State == tracker.ControlPending {
		c.State = tracker.ControlRestored
		c.Result = &tracker.ControlResult{Code: "tracker_restored_inactive"}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return MetaKV{}, tracker.ErrInvalid
	}
	record.Value = string(raw)
	return record, nil
}

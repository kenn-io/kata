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
	var admissionErr error
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		result = tracker.ControlReceipt{}
		fresh = false
		admissionErr = nil
		if err := a.authorize(ctx, tx, p.ProjectUID); err != nil {
			return err
		}
		capacity, err := loadTrackerControlCapacity(ctx, tx)
		if err != nil {
			return err
		}
		key := TrackerControlKey(a.InstanceUID, p.ProjectUID, p.UID)
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
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
		if capacity.full(p) {
			// Commit a newly rebuilt counter even when admission is full.
			admissionErr = tracker.ErrControlCapacity
			return nil
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
			capacity.change(p, 1)
			if err := saveTrackerControlCapacity(ctx, tx, capacity); err != nil {
				return err
			}
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
	if err != nil {
		return result, fresh, err
	}
	return result, fresh, admissionErr
}

// TrackerControlCapacityKey is local, rebuildable admission bookkeeping.
const TrackerControlCapacityKey = TrackerMetadataPrefix + "control_capacity.global"

type trackerControlCounts struct {
	Pending     int            `json:"pending"`
	Projects    map[string]int `json:"projects"`
	Connections map[string]int `json:"connections"`
}

func (c *trackerControlCounts) change(p tracker.ControlReceipt, delta int) {
	c.Pending += delta
	c.Projects[p.ProjectUID] += delta
	connection := p.Selection.OwnerInstanceUID + "." + p.Selection.ConnectionUID
	c.Connections[connection] += delta
	if c.Projects[p.ProjectUID] == 0 {
		delete(c.Projects, p.ProjectUID)
	}
	if c.Connections[connection] == 0 {
		delete(c.Connections, connection)
	}
}
func (c trackerControlCounts) full(p tracker.ControlReceipt) bool {
	return c.Pending >= 256 || c.Projects[p.ProjectUID] >= 64 || c.Connections[p.Selection.OwnerInstanceUID+"."+p.Selection.ConnectionUID] >= 64
}

// SQLite retries conflicting writes; PostgreSQL retries serializable transactions.
// The counter and receipt changes commit together. A missing row can be an
// upgrade from before counters existed, so rebuild from retained receipts.
func loadTrackerControlCapacity(ctx context.Context, tx *sql.Tx) (trackerControlCounts, error) {
	c := trackerControlCounts{Projects: map[string]int{}, Connections: map[string]int{}}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, TrackerControlCapacityKey, "")
	if err != nil {
		return c, err
	}
	n, err := inserted.RowsAffected()
	if err != nil {
		return c, err
	}
	if n == 0 {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, TrackerControlCapacityKey).Scan(&raw); err != nil {
			return c, err
		}
		if json.Unmarshal([]byte(raw), &c, json.RejectUnknownMembers(true)) != nil || c.Projects == nil || c.Connections == nil || c.Pending < 0 {
			return c, tracker.ErrInvalid
		}
		return c, nil
	}
	prefix := TrackerMetadataPrefix + "control."
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM meta WHERE key >= $1 AND key < $2`, prefix, TrackerMetadataPrefix+"control/")
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return c, err
		}
		receipt, err := decodeTrackerControl(key, raw)
		if err != nil {
			return c, err
		}
		if receipt.State == tracker.ControlPending {
			c.change(receipt, 1)
		}
	}
	if err := rows.Err(); err != nil {
		return c, err
	}
	if err := rows.Close(); err != nil {
		return c, err
	}
	if err := saveTrackerControlCapacity(ctx, tx, c); err != nil {
		return c, err
	}
	return c, nil
}
func saveTrackerControlCapacity(ctx context.Context, tx *sql.Tx, c trackerControlCounts) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(raw), TrackerControlCapacityKey)
	return err
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
		capacity, err := loadTrackerControlCapacity(ctx, tx)
		if err != nil {
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
		if capacity.Pending <= 0 || capacity.Projects[previous.ProjectUID] <= 0 || capacity.Connections[previous.Selection.OwnerInstanceUID+"."+previous.Selection.ConnectionUID] <= 0 {
			return tracker.ErrInvalid
		}
		capacity.change(previous, -1)
		return saveTrackerControlCapacity(ctx, tx, capacity)
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

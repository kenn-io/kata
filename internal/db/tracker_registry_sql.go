package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"maps"
	"strings"

	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

type TrackerRegistryStore interface {
	ReconcileTrackerRegistry(context.Context, string, []tracker.RegistryPolicy) ([]tracker.RegistryRecord, error)
}

func TrackerRegistryKey(owner, connection string) string {
	return TrackerMetadataPrefix + "registry." + owner + "." + connection
}

// ReconcileRegistry is an owner-local startup/reload operation, never an HTTP
// mutation. All removals and replacements commit together before publication of
// the in-memory snapshot. Native transaction hooks remain in effect.
func (a TrackerFenceSQL) ReconcileRegistry(ctx context.Context, epoch string, policies []tracker.RegistryPolicy) ([]tracker.RegistryRecord, error) {
	if !uid.Valid(epoch) || !uid.Valid(a.InstanceUID) || len(policies) > 128 {
		return nil, tracker.ErrInvalid
	}
	supplied := map[string]tracker.RegistryPolicy{}
	for _, p := range policies {
		if p.Validate() != nil || p.Selection.OwnerInstanceUID != a.InstanceUID {
			return nil, tracker.ErrInvalid
		}
		if _, ok := supplied[p.Selection.ConnectionUID]; ok {
			return nil, tracker.ErrInvalid
		}
		supplied[p.Selection.ConnectionUID] = p
	}
	var output []tracker.RegistryRecord
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		output = nil
		pending := maps.Clone(supplied)
		prefix := TrackerMetadataPrefix + "registry." + a.InstanceUID + "."
		rows, err := tx.QueryContext(ctx, `SELECT key,value FROM meta WHERE substr(key,1,$1)=$2`, len(prefix), prefix)
		if err != nil {
			return err
		}
		stored := map[string]tracker.RegistryRecord{}
		for rows.Next() {
			var key, raw string
			if err := rows.Scan(&key, &raw); err != nil {
				_ = rows.Close()
				return err
			}
			var record tracker.RegistryRecord
			if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &record, json.RejectUnknownMembers(true)) != nil || record.Validate() != nil || key != TrackerRegistryKey(record.Policy.Selection.OwnerInstanceUID, record.Policy.Selection.ConnectionUID) || len(stored) >= 1024 {
				_ = rows.Close()
				return tracker.ErrInvalid
			}
			stored[record.Policy.Selection.ConnectionUID] = record
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(stored)+len(pending) > 1152 {
			return tracker.ErrHistoryFull
		}
		for connection, old := range stored {
			var configured *tracker.RegistryPolicy
			if p, ok := pending[connection]; ok {
				configured = &p
				delete(pending, connection)
			}
			next, err := tracker.NextRegistryRecord(&old, configured, epoch)
			if err != nil {
				return err
			}
			if err := writeTrackerRegistryRecord(ctx, tx, next); err != nil {
				return err
			}
			if configured != nil {
				output = append(output, next)
			}
		}
		if len(stored)+len(pending) > 1024 {
			return tracker.ErrHistoryFull
		}
		for _, p := range pending {
			next, err := tracker.NextRegistryRecord(nil, &p, epoch)
			if err != nil {
				return err
			}
			if err := writeTrackerRegistryRecord(ctx, tx, next); err != nil {
				return err
			}
			output = append(output, next)
		}
		return nil
	})
	return output, err
}
func writeTrackerRegistryRecord(ctx context.Context, tx *sql.Tx, record tracker.RegistryRecord) error {
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 1<<20 {
		return tracker.ErrInvalid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, TrackerRegistryKey(record.Policy.Selection.OwnerInstanceUID, record.Policy.Selection.ConnectionUID), string(raw))
	return err
}
func trackerRegistryMetadataForImport(record MetaKV) (MetaKV, error) {
	var r tracker.RegistryRecord
	if len(record.Value) > 1<<20 || json.Unmarshal([]byte(record.Value), &r, json.RejectUnknownMembers(true)) != nil || r.Validate() != nil || record.Key != TrackerRegistryKey(r.Policy.Selection.OwnerInstanceUID, r.Policy.Selection.ConnectionUID) {
		return MetaKV{}, ErrTrackerFenceInvalid
	}
	r.State = tracker.RegistryRestored
	raw, err := json.Marshal(r)
	if err != nil {
		return MetaKV{}, ErrTrackerFenceInvalid
	}
	record.Value = string(raw)
	return record, nil
}
func isTrackerRegistryKey(key string) bool {
	return strings.HasPrefix(key, TrackerMetadataPrefix+"registry.")
}

// ValidateTrackerRegistryInTransaction joins owner registry liveness to dispatch.
// A previous in-memory read cannot authorize admission after a committed revoke.
func (a TrackerFenceSQL) ValidateTrackerRegistryInTransaction(ctx context.Context, tx *sql.Tx, selection tracker.Selection, provider, epoch string) error {
	if selection.OwnerInstanceUID != a.InstanceUID || selection.Validate() != nil || !uid.Valid(epoch) {
		return tracker.ErrUnavailable
	}
	var raw string
	key := TrackerRegistryKey(a.InstanceUID, selection.ConnectionUID)
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tracker.ErrUnavailable
		}
		return err
	}
	var record tracker.RegistryRecord
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &record, json.RejectUnknownMembers(true)) != nil || record.Validate() != nil {
		return tracker.ErrUnavailable
	}
	if record.State != tracker.RegistryActive || record.ProcessEpoch != epoch || record.Policy.Selection != selection || record.Policy.Provider != provider {
		return tracker.ErrUnavailable
	}
	return nil
}

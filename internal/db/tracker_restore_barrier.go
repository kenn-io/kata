package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// TrackerRestoreBarrier records that a same-instance full restore may omit
// later provider history, even if its snapshot predates every named binding.
// This is local full-only metadata, never a provider acknowledgement or grant.
type TrackerRestoreBarrier struct {
	Version         int    `json:"version"`
	RootInstanceUID string `json:"root_instance_uid"`
	ProjectUID      string `json:"project_uid,omitempty"`
}

func trackerRestoreBarrierKey(root, project string) string {
	key := TrackerMetadataPrefix + "restore_barrier." + root
	if project != "" {
		key += "." + project
	}
	return key
}
func isTrackerRestoreBarrierKey(key string) bool {
	return strings.HasPrefix(key, TrackerMetadataPrefix+"restore_barrier.")
}
func trackerRestoreBarrierForImport(record MetaKV) (MetaKV, error) {
	var b TrackerRestoreBarrier
	if len(record.Value) > 1024 || json.Unmarshal([]byte(record.Value), &b, json.RejectUnknownMembers(true)) != nil || b.Version != 1 || !uid.Valid(b.RootInstanceUID) || (b.ProjectUID != "" && !uid.Valid(b.ProjectUID)) || record.Key != trackerRestoreBarrierKey(b.RootInstanceUID, b.ProjectUID) {
		return MetaKV{}, ErrTrackerFenceInvalid
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return MetaKV{}, err
	}
	record.Value = string(raw)
	return record, nil
}
func trackerRestoreBarrierPresentSQL(ctx context.Context, tx *sql.Tx, root, project string) (bool, error) {
	keys := []string{trackerRestoreBarrierKey(root, "")}
	if project != "" {
		keys = append(keys, trackerRestoreBarrierKey(root, project))
	}
	for _, key := range keys {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		if _, err := trackerRestoreBarrierForImport(MetaKV{Key: key, Value: raw}); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
func writeTrackerRestoreBarrierSQL(ctx context.Context, tx *sql.Tx, root, project string) error {
	if !uid.Valid(root) || (project != "" && !uid.Valid(project)) {
		return ErrTrackerFenceInvalid
	}
	raw, err := json.Marshal(TrackerRestoreBarrier{Version: 1, RootInstanceUID: root, ProjectUID: project})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, trackerRestoreBarrierKey(root, project), string(raw))
	return err
}

// TrackerRestoreStateStore reports whether restored history blocks writes.
type TrackerRestoreStateStore interface {
	TrackerRestoreHistoryBlocked(context.Context, string) (bool, error)
}

// RestoreHistoryBlocked reports a retained history barrier for a project.
func (a TrackerFenceSQL) RestoreHistoryBlocked(ctx context.Context, project string) (bool, error) {
	blocked := false
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		blocked = false
		if err := a.authorize(ctx, tx, project); err != nil {
			return err
		}
		var err error
		blocked, err = trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, project)
		if err != nil || blocked {
			return err
		}
		key := TrackerActivationKey(a.InstanceUID, project)
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var activation TrackerActivation
		if len(raw) > 8192 || json.Unmarshal([]byte(raw), &activation, json.RejectUnknownMembers(true)) != nil || activation.Validate() != nil || activation.RootInstanceUID != a.InstanceUID || activation.ProjectUID != project {
			return ErrTrackerFenceInvalid
		}
		blocked = activation.RestoredHistoryGap
		return nil
	})
	return blocked, err
}

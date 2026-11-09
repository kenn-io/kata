package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

// IssueSyncBindingFence locks and checks the exact authorized binding before a
// claim or disable. Call it only for that transaction; later provider work may
// advance the binding revision under its own claim fence.
func IssueSyncBindingFence(expected IssueSyncBinding) TransactionFence {
	expected.Config = bytes.Clone(expected.Config)
	if expected.ConnectionUID != nil {
		marker := *expected.ConnectionUID
		expected.ConnectionUID = &marker
	}
	return func(ctx context.Context, tx Transaction) error {
		if expected.ID <= 0 || expected.UpdatedAt.IsZero() || ValidateIssueSyncConnectionMarker(expected.ConnectionUID, expected.Config) != nil {
			return ErrIssueSyncBindingChanged
		}
		result, err := tx.ExecContext(ctx, `UPDATE issue_sync_bindings SET updated_at=updated_at WHERE id=$1`, expected.ID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrIssueSyncBindingChanged
		}
		var project int64
		var provider, source, remote, config, updated string
		var marker sql.NullString
		var enabled bool
		err = tx.QueryRowContext(ctx, `SELECT project_id,provider,source_key,remote_id,config_json,connection_uid,enabled,CAST(updated_at AS TEXT) FROM issue_sync_bindings WHERE id=$1`, expected.ID).Scan(&project, &provider, &source, &remote, &config, &marker, &enabled, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrIssueSyncBindingChanged
		}
		if err != nil {
			return err
		}
		var revisionMatches bool
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
			if stamp, err := time.Parse(layout, updated); err == nil && stamp.Equal(expected.UpdatedAt) {
				revisionMatches = true
				break
			}
		}
		if !revisionMatches || project != expected.ProjectID || provider != expected.Provider || source != expected.SourceKey || remote != expected.RemoteID || config != string(expected.Config) || enabled != expected.Enabled {
			return ErrIssueSyncBindingChanged
		}
		if expected.ConnectionUID == nil {
			if marker.Valid {
				return ErrIssueSyncBindingChanged
			}
		} else if !marker.Valid || marker.String != *expected.ConnectionUID {
			return ErrIssueSyncBindingChanged
		}
		return nil
	}
}

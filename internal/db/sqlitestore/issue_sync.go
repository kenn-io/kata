package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

// UpsertIssueSyncBinding creates or re-enables the issue sync binding for a
// project. A project may only ever bind to one provider source in v1.
func (d *Store) UpsertIssueSyncBinding(ctx context.Context, p db.UpsertIssueSyncBindingParams) (db.IssueSyncBinding, error) {
	if err := validateIssueSyncBindingParams(p); err != nil {
		return db.IssueSyncBinding{}, err
	}
	return retryWrite1(ctx, d, func() (db.IssueSyncBinding, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("begin issue sync upsert: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if err := requireActiveProject(ctx, tx, p.ProjectID); err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := rejectFederationSpokeIssueSyncProject(ctx, tx, p.ProjectID); err != nil {
			return db.IssueSyncBinding{}, err
		}

		existing, err := issueSyncBindingByProject(ctx, tx, p.ProjectID)
		if err == nil {
			if existing.Provider != p.Provider || existing.RemoteID != p.RemoteID {
				return db.IssueSyncBinding{}, db.ErrIssueSyncProjectAlreadyBound
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE issue_sync_bindings
				   SET display_name = ?,
				       last_cursor_at = CASE WHEN config_json <> ? THEN NULL ELSE last_cursor_at END,
				       config_json = ?,
				       enabled = 1, interval_seconds = ?,
				       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
				 WHERE id = ?`,
				p.DisplayName, string(p.Config), string(p.Config), p.IntervalSeconds, existing.ID); err != nil {
				return db.IssueSyncBinding{}, fmt.Errorf("update issue sync binding: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO issue_sync_status(binding_id, project_id)
				VALUES(?, ?)
				ON CONFLICT(binding_id) DO NOTHING`, existing.ID, existing.ProjectID); err != nil {
				return db.IssueSyncBinding{}, fmt.Errorf("ensure issue sync status: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE issue_sync_status
				   SET sync_started_at = NULL
				 WHERE binding_id = ?`, existing.ID); err != nil {
				return db.IssueSyncBinding{}, fmt.Errorf("clear issue sync claim: %w", err)
			}
			updated, err := issueSyncBindingByID(ctx, tx, existing.ID)
			if err != nil {
				return db.IssueSyncBinding{}, err
			}
			if err := tx.Commit(); err != nil {
				return db.IssueSyncBinding{}, fmt.Errorf("commit issue sync upsert: %w", err)
			}
			return updated, nil
		}
		if !errors.Is(err, db.ErrNotFound) {
			return db.IssueSyncBinding{}, err
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO issue_sync_bindings(
				project_id, provider, source_key, remote_id, display_name,
				config_json, enabled, interval_seconds
			) VALUES(?, ?, ?, ?, ?, ?, 1, ?)`,
			p.ProjectID, p.Provider, p.SourceKey, p.RemoteID, p.DisplayName,
			string(p.Config), p.IntervalSeconds)
		if err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("insert issue sync binding: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("read issue sync binding id: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO issue_sync_status(binding_id, project_id)
			VALUES(?, ?)`, id, p.ProjectID); err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("insert issue sync status: %w", err)
		}
		binding, err := issueSyncBindingByID(ctx, tx, id)
		if err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("commit issue sync upsert: %w", err)
		}
		return binding, nil
	})
}

// DisableIssueSyncBinding marks a project's binding disabled without
// removing status rows or import mappings.
func (d *Store) DisableIssueSyncBinding(ctx context.Context, projectID int64) (db.IssueSyncBinding, error) {
	return retryWrite1(ctx, d, func() (db.IssueSyncBinding, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("begin issue sync disable: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		binding, err := issueSyncBindingByProject(ctx, tx, projectID)
		if errors.Is(err, db.ErrNotFound) {
			return db.IssueSyncBinding{}, db.ErrIssueSyncNotEnabled
		}
		if err != nil {
			return db.IssueSyncBinding{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE issue_sync_bindings
			   SET enabled = 0, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`, binding.ID); err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("disable issue sync binding: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE issue_sync_status
			   SET sync_started_at = NULL
			 WHERE binding_id = ?`, binding.ID); err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("clear issue sync claim: %w", err)
		}
		disabled, err := issueSyncBindingByID(ctx, tx, binding.ID)
		if err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("commit issue sync disable: %w", err)
		}
		return disabled, nil
	})
}

// IssueSyncBindingByProject returns the binding for one project.
func (d *Store) IssueSyncBindingByProject(ctx context.Context, projectID int64) (db.IssueSyncBinding, error) {
	return issueSyncBindingByProject(ctx, d, projectID)
}

// IssueSyncBindingByID returns one binding by row id.
func (d *Store) IssueSyncBindingByID(ctx context.Context, bindingID int64) (db.IssueSyncBinding, error) {
	return issueSyncBindingByID(ctx, d, bindingID)
}

// IssueSyncStatusByProject returns runner status for one project.
func (d *Store) IssueSyncStatusByProject(ctx context.Context, projectID int64) (db.IssueSyncStatus, error) {
	return issueSyncStatusByProject(ctx, d, projectID)
}

// ListDueIssueSyncBindings returns enabled, non-archived provider bindings whose
// attempt interval has elapsed and whose in-flight claim is either absent or
// stale enough for recovery.
func (d *Store) ListDueIssueSyncBindings(ctx context.Context, provider string, now, staleBefore time.Time, limit int) ([]db.IssueSyncBinding, error) {
	out := []db.IssueSyncBinding{}
	if limit <= 0 {
		return out, nil
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, fmt.Errorf("%w: issue sync provider is required", db.ErrImportValidation)
	}
	rows, err := d.QueryContext(ctx, issueSyncBindingSelect+`
		  JOIN issue_sync_status s ON s.binding_id = b.id
		  JOIN projects p ON p.id = b.project_id
		 WHERE b.provider = ?
		   AND b.enabled = 1
		   AND p.deleted_at IS NULL
		   AND (s.sync_started_at IS NULL OR s.sync_started_at < ?)
		   AND (
		       s.last_attempt_at IS NULL
		       OR ((julianday(?) - julianday(s.last_attempt_at)) * 86400.0) >= b.interval_seconds
		   )
		 ORDER BY COALESCE(s.last_attempt_at, ''), b.id
		 LIMIT ?`, provider, staleBefore.UTC().Format(sqliteTimeFormat), now.UTC().Format(sqliteTimeFormat), limit)
	if err != nil {
		return nil, fmt.Errorf("list due issue sync bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		binding, err := scanIssueSyncBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due issue sync bindings: %w", err)
	}
	return out, nil
}

// ClaimIssueSyncBinding marks a provider binding in-flight unless a non-stale
// run is already active.
func (d *Store) ClaimIssueSyncBinding(ctx context.Context, bindingID int64, provider string, now, staleBefore time.Time) (db.IssueSyncBinding, bool, error) {
	return retryWrite2(ctx, d, func() (db.IssueSyncBinding, bool, error) {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			return db.IssueSyncBinding{}, false, fmt.Errorf("%w: issue sync provider is required", db.ErrImportValidation)
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueSyncBinding{}, false, fmt.Errorf("begin issue sync claim: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		binding, err := issueSyncBindingByID(ctx, tx, bindingID)
		if err != nil {
			return db.IssueSyncBinding{}, false, err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE issue_sync_status
			   SET sync_started_at = ?, last_attempt_at = ?
			 WHERE binding_id = ?
			   AND EXISTS (
			       SELECT 1
			         FROM issue_sync_bindings gb
			         JOIN projects p ON p.id = gb.project_id
			        WHERE gb.id = issue_sync_status.binding_id
			          AND gb.provider = ?
			          AND gb.enabled = 1
			          AND p.deleted_at IS NULL
			   )
			   AND (sync_started_at IS NULL OR sync_started_at < ?)`,
			now.UTC().Format(sqliteTimeFormat), now.UTC().Format(sqliteTimeFormat),
			binding.ID, provider, staleBefore.UTC().Format(sqliteTimeFormat))
		if err != nil {
			return db.IssueSyncBinding{}, false, fmt.Errorf("claim issue sync binding: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return db.IssueSyncBinding{}, false, fmt.Errorf("read issue sync claim affected rows: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return db.IssueSyncBinding{}, false, fmt.Errorf("commit issue sync claim: %w", err)
		}
		return binding, affected > 0, nil
	})
}

// RecordIssueSyncSuccess clears in-flight state, advances the cursor, and
// records result counts.
func (d *Store) RecordIssueSyncSuccess(ctx context.Context, p db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	return retryWrite1(ctx, d, func() (db.IssueSyncStatus, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("begin issue sync success: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := issueSyncBindingByID(ctx, tx, p.BindingID); err != nil {
			return db.IssueSyncStatus{}, err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE issue_sync_status
			   SET sync_started_at = NULL,
			       last_success_at = ?,
			       last_error_at = NULL,
			       last_error = NULL,
			       last_created = ?,
			       last_updated = ?,
			       last_unchanged = ?,
			       last_comments = ?
			 WHERE binding_id = ?
			   AND sync_started_at = ?`,
			p.At.UTC().Format(sqliteTimeFormat), p.LastCreated, p.LastUpdated,
			p.LastUnchanged, p.LastComments, p.BindingID,
			p.StartedAt.UTC().Format(sqliteTimeFormat))
		if err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("record issue sync success: %w", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("read issue sync success affected rows: %w", err)
		} else if affected == 0 {
			return db.IssueSyncStatus{}, db.ErrIssueSyncAlreadyRunning
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE issue_sync_bindings
			   SET last_cursor_at = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`,
			p.CursorAt.UTC().Format(sqliteTimeFormat), p.BindingID); err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("advance issue sync cursor: %w", err)
		}
		status, err := issueSyncStatusByBinding(ctx, tx, p.BindingID)
		if err != nil {
			return db.IssueSyncStatus{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("commit issue sync success: %w", err)
		}
		return status, nil
	})
}

// RecordIssueSyncError clears in-flight state and records the latest failure.
func (d *Store) RecordIssueSyncError(ctx context.Context, p db.IssueSyncErrorParams) (db.IssueSyncStatus, error) {
	return retryWrite1(ctx, d, func() (db.IssueSyncStatus, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("begin issue sync error: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := issueSyncBindingByID(ctx, tx, p.BindingID); err != nil {
			return db.IssueSyncStatus{}, err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE issue_sync_status
			   SET sync_started_at = NULL,
			       last_error_at = ?,
			       last_error = ?
			 WHERE binding_id = ?
			   AND sync_started_at = ?`,
			p.At.UTC().Format(sqliteTimeFormat), p.Error, p.BindingID,
			p.StartedAt.UTC().Format(sqliteTimeFormat))
		if err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("record issue sync error: %w", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("read issue sync error affected rows: %w", err)
		} else if affected == 0 {
			return db.IssueSyncStatus{}, db.ErrIssueSyncAlreadyRunning
		}
		status, err := issueSyncStatusByBinding(ctx, tx, p.BindingID)
		if err != nil {
			return db.IssueSyncStatus{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueSyncStatus{}, fmt.Errorf("commit issue sync error: %w", err)
		}
		return status, nil
	})
}

// RefreshIssueSyncBinding updates mutable provider display/config metadata.
func (d *Store) RefreshIssueSyncBinding(ctx context.Context, p db.IssueSyncBindingUpdateParams) (db.IssueSyncBinding, error) {
	if strings.TrimSpace(p.DisplayName) == "" || !json.Valid(p.Config) {
		return db.IssueSyncBinding{}, fmt.Errorf("%w: issue sync binding requires display name and valid config JSON", db.ErrImportValidation)
	}
	return retryWrite1(ctx, d, func() (db.IssueSyncBinding, error) {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("begin issue sync binding refresh: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		res, err := tx.ExecContext(ctx, `
			UPDATE issue_sync_bindings
			   SET display_name = ?, config_json = ?,
			       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`,
			p.DisplayName, string(p.Config), p.BindingID)
		if err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("refresh issue sync binding: %w", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("read issue sync refresh affected rows: %w", err)
		} else if affected == 0 {
			return db.IssueSyncBinding{}, db.ErrNotFound
		}
		binding, err := issueSyncBindingByID(ctx, tx, p.BindingID)
		if err != nil {
			return db.IssueSyncBinding{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("commit issue sync binding refresh: %w", err)
		}
		return binding, nil
	})
}

func validateIssueSyncBindingParams(p db.UpsertIssueSyncBindingParams) error {
	if p.ProjectID <= 0 ||
		strings.TrimSpace(p.Provider) == "" ||
		strings.TrimSpace(p.SourceKey) == "" ||
		strings.TrimSpace(p.RemoteID) == "" ||
		strings.TrimSpace(p.DisplayName) == "" ||
		p.IntervalSeconds <= 0 ||
		!json.Valid(p.Config) {
		return fmt.Errorf("%w: invalid issue sync binding", db.ErrImportValidation)
	}
	return nil
}

func requireActiveProject(ctx context.Context, q queryer, projectID int64) error {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM projects WHERE id = ? AND deleted_at IS NULL`, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read issue sync project: %w", err)
	}
	return nil
}

func rejectFederationSpokeIssueSyncProject(ctx context.Context, q queryer, projectID int64) error {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT project_id FROM federation_bindings WHERE project_id = ? AND role = ? AND enabled = 1`,
		projectID, string(db.FederationRoleSpoke)).Scan(&id)
	if err == nil {
		return db.ErrIssueSyncFederationBinding
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("check issue sync federation binding: %w", err)
}

func rejectIssueSyncedFederationProject(ctx context.Context, q queryer, projectID int64) error {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT project_id FROM issue_sync_bindings WHERE project_id = ? AND enabled = 1`, projectID).Scan(&id)
	if err == nil {
		return db.ErrIssueSyncFederationBinding
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("check federation issue sync binding: %w", err)
}

func issueSyncBindingByProject(ctx context.Context, q queryer, projectID int64) (db.IssueSyncBinding, error) {
	return scanIssueSyncBinding(q.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.project_id = ?`, projectID))
}

func issueSyncBindingByID(ctx context.Context, q queryer, bindingID int64) (db.IssueSyncBinding, error) {
	return scanIssueSyncBinding(q.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.id = ?`, bindingID))
}

const issueSyncBindingSelect = `SELECT b.id, b.project_id, b.provider, b.source_key,
       b.remote_id, b.display_name, b.config_json, b.enabled, b.interval_seconds, b.last_cursor_at,
       b.created_at, b.updated_at
  FROM issue_sync_bindings b`

func scanIssueSyncBinding(r rowScanner) (db.IssueSyncBinding, error) {
	var (
		binding      db.IssueSyncBinding
		enabled      int
		lastCursorAt sql.NullTime
		config       string
	)
	err := r.Scan(&binding.ID, &binding.ProjectID, &binding.Provider, &binding.SourceKey,
		&binding.RemoteID, &binding.DisplayName, &config,
		&enabled, &binding.IntervalSeconds, &lastCursorAt,
		&binding.CreatedAt, &binding.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueSyncBinding{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueSyncBinding{}, fmt.Errorf("scan issue sync binding: %w", err)
	}
	binding.Config = json.RawMessage(config)
	binding.Enabled = enabled == 1
	if lastCursorAt.Valid {
		binding.LastCursorAt = &lastCursorAt.Time
	}
	return binding, nil
}

func issueSyncStatusByProject(ctx context.Context, q queryer, projectID int64) (db.IssueSyncStatus, error) {
	return scanIssueSyncStatus(q.QueryRowContext(ctx, issueSyncStatusSelect+` WHERE project_id = ?`, projectID))
}

func issueSyncStatusByBinding(ctx context.Context, q queryer, bindingID int64) (db.IssueSyncStatus, error) {
	return scanIssueSyncStatus(q.QueryRowContext(ctx, issueSyncStatusSelect+` WHERE binding_id = ?`, bindingID))
}

const issueSyncStatusSelect = `SELECT binding_id, project_id, sync_started_at, last_attempt_at,
       last_success_at, last_error_at, last_error, last_created,
       last_updated, last_unchanged, last_comments
  FROM issue_sync_status`

func scanIssueSyncStatus(r rowScanner) (db.IssueSyncStatus, error) {
	var (
		status      db.IssueSyncStatus
		syncStarted sql.NullTime
		lastAttempt sql.NullTime
		lastSuccess sql.NullTime
		lastErrorAt sql.NullTime
		lastError   sql.NullString
	)
	err := r.Scan(&status.BindingID, &status.ProjectID, &syncStarted, &lastAttempt,
		&lastSuccess, &lastErrorAt, &lastError, &status.LastCreated,
		&status.LastUpdated, &status.LastUnchanged, &status.LastComments)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueSyncStatus{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueSyncStatus{}, fmt.Errorf("scan issue sync status: %w", err)
	}
	if syncStarted.Valid {
		status.SyncStartedAt = &syncStarted.Time
	}
	if lastAttempt.Valid {
		status.LastAttemptAt = &lastAttempt.Time
	}
	if lastSuccess.Valid {
		status.LastSuccessAt = &lastSuccess.Time
	}
	if lastErrorAt.Valid {
		status.LastErrorAt = &lastErrorAt.Time
	}
	if lastError.Valid {
		status.LastError = lastError.String
	}
	return status, nil
}

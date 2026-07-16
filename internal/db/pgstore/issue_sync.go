package pgstore

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

const issueSyncBindingSelect = `SELECT b.id, b.project_id, b.provider, b.source_key,
       b.remote_id, b.display_name, b.config_json, b.enabled, b.interval_seconds,
       b.last_cursor_at, b.created_at, b.updated_at
  FROM issue_sync_bindings b`

const issueSyncStatusSelect = `SELECT binding_id, project_id, sync_started_at, last_attempt_at,
       last_success_at, last_error_at, last_error, last_created,
       last_updated, last_unchanged, last_comments
  FROM issue_sync_status`

// UpsertIssueSyncBinding creates or re-enables one project's immutable remote
// identity while refreshing its mutable configuration.
func (s *Store) UpsertIssueSyncBinding(
	ctx context.Context,
	params db.UpsertIssueSyncBindingParams,
) (db.IssueSyncBinding, error) {
	if err := validateIssueSyncBindingParams(params); err != nil {
		return db.IssueSyncBinding{}, err
	}
	var binding db.IssueSyncBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if _, err := scanProject(tx.QueryRowContext(ctx,
			projectSelect+` WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, params.ProjectID)); err != nil {
			return err
		}
		if err := rejectFederationSpokeIssueSyncProject(ctx, tx, params.ProjectID); err != nil {
			return err
		}
		existing, err := scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.project_id=$1 FOR UPDATE`, params.ProjectID))
		if err == nil {
			if existing.Provider != params.Provider || existing.RemoteID != params.RemoteID {
				return db.ErrIssueSyncProjectAlreadyBound
			}
			updatedAt := nowStoredTimestamp()
			if _, err := tx.ExecContext(ctx, `UPDATE issue_sync_bindings SET
 display_name=$1, last_cursor_at=CASE WHEN config_json <> $2 THEN NULL ELSE last_cursor_at END,
 config_json=$2, enabled=1, interval_seconds=$3, updated_at=$4 WHERE id=$5`,
				params.DisplayName, string(params.Config), params.IntervalSeconds, updatedAt, existing.ID,
			); err != nil {
				return mapSQLError(err, nil)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO issue_sync_status(binding_id, project_id)
VALUES($1,$2) ON CONFLICT(binding_id) DO NOTHING`, existing.ID, existing.ProjectID); err != nil {
				return mapSQLError(err, nil)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE issue_sync_status SET sync_started_at=NULL WHERE binding_id=$1`, existing.ID,
			); err != nil {
				return mapSQLError(err, nil)
			}
			binding, err = scanIssueSyncBinding(tx.QueryRowContext(ctx,
				issueSyncBindingSelect+` WHERE b.id=$1`, existing.ID))
			return err
		}
		if !errors.Is(err, db.ErrNotFound) {
			return err
		}
		var bindingID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO issue_sync_bindings(
 project_id, provider, source_key, remote_id, display_name, config_json, enabled, interval_seconds
) VALUES($1,$2,$3,$4,$5,$6,1,$7) RETURNING id`,
			params.ProjectID, params.Provider, params.SourceKey, params.RemoteID,
			params.DisplayName, string(params.Config), params.IntervalSeconds,
		).Scan(&bindingID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO issue_sync_status(binding_id, project_id) VALUES($1,$2)`, bindingID, params.ProjectID,
		); err != nil {
			return mapSQLError(err, nil)
		}
		binding, err = scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.id=$1`, bindingID))
		return err
	})
	return binding, err
}

// DisableIssueSyncBinding disables one project binding and releases its claim.
func (s *Store) DisableIssueSyncBinding(ctx context.Context, projectID int64) (db.IssueSyncBinding, error) {
	var binding db.IssueSyncBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		current, err := scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.project_id=$1 FOR UPDATE`, projectID))
		if errors.Is(err, db.ErrNotFound) {
			return db.ErrIssueSyncNotEnabled
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issue_sync_bindings
SET enabled=0, updated_at=$1 WHERE id=$2`, nowStoredTimestamp(), current.ID); err != nil {
			return mapSQLError(err, nil)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE issue_sync_status SET sync_started_at=NULL WHERE binding_id=$1`, current.ID,
		); err != nil {
			return mapSQLError(err, nil)
		}
		binding, err = scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.id=$1`, current.ID))
		return err
	})
	return binding, err
}

// IssueSyncBindingByProject returns the binding for one project.
func (s *Store) IssueSyncBindingByProject(ctx context.Context, projectID int64) (db.IssueSyncBinding, error) {
	return scanIssueSyncBinding(s.QueryRowContext(ctx,
		issueSyncBindingSelect+` WHERE b.project_id=$1`, projectID))
}

// IssueSyncBindingByID returns one binding by row identity.
func (s *Store) IssueSyncBindingByID(ctx context.Context, bindingID int64) (db.IssueSyncBinding, error) {
	return scanIssueSyncBinding(s.QueryRowContext(ctx, issueSyncBindingSelect+` WHERE b.id=$1`, bindingID))
}

// IssueSyncStatusByProject returns runner state for one project.
func (s *Store) IssueSyncStatusByProject(ctx context.Context, projectID int64) (db.IssueSyncStatus, error) {
	return scanIssueSyncStatus(s.QueryRowContext(ctx, issueSyncStatusSelect+` WHERE project_id=$1`, projectID))
}

// ListDueIssueSyncBindings returns claimable bindings whose interval elapsed.
func (s *Store) ListDueIssueSyncBindings(
	ctx context.Context,
	provider string,
	now time.Time,
	staleBefore time.Time,
	limit int,
) ([]db.IssueSyncBinding, error) {
	if limit <= 0 {
		return []db.IssueSyncBinding{}, nil
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, fmt.Errorf("%w: issue sync provider is required", db.ErrImportValidation)
	}
	rows, err := s.QueryContext(ctx, issueSyncBindingSelect+`
  JOIN issue_sync_status status ON status.binding_id=b.id
  JOIN projects project ON project.id=b.project_id
 WHERE b.provider=$1 AND b.enabled=1 AND project.deleted_at IS NULL
   AND (status.sync_started_at IS NULL OR status.sync_started_at < $2)
   AND (status.last_attempt_at IS NULL OR
        status.last_attempt_at::timestamptz + make_interval(secs => b.interval_seconds) <= $3::timestamptz)
 ORDER BY COALESCE(status.last_attempt_at, ''), b.id
 LIMIT $4`, provider, formatStoredTime(staleBefore), formatStoredTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list due issue sync bindings: %w", mapSQLError(err, nil))
	}
	defer func() { _ = rows.Close() }()
	bindings := make([]db.IssueSyncBinding, 0)
	for rows.Next() {
		binding, err := scanIssueSyncBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due issue sync bindings: %w", mapSQLError(err, nil))
	}
	return bindings, nil
}

// ClaimIssueSyncBinding claims one enabled binding unless its current claim is
// still fresh.
func (s *Store) ClaimIssueSyncBinding(
	ctx context.Context,
	bindingID int64,
	provider string,
	now time.Time,
	staleBefore time.Time,
) (db.IssueSyncBinding, bool, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return db.IssueSyncBinding{}, false,
			fmt.Errorf("%w: issue sync provider is required", db.ErrImportValidation)
	}
	var binding db.IssueSyncBinding
	var claimed bool
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		binding, err = scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.id=$1`, bindingID))
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE issue_sync_status SET
 sync_started_at=$1, last_attempt_at=$1
WHERE binding_id=$2
  AND EXISTS(SELECT 1 FROM issue_sync_bindings candidate
    JOIN projects project ON project.id=candidate.project_id
    WHERE candidate.id=issue_sync_status.binding_id AND candidate.provider=$3
      AND candidate.enabled=1 AND project.deleted_at IS NULL)
  AND (sync_started_at IS NULL OR sync_started_at < $4)`,
			formatStoredTime(now), binding.ID, provider, formatStoredTime(staleBefore))
		if err != nil {
			return mapSQLError(err, nil)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		claimed = affected > 0
		return nil
	})
	return binding, claimed, err
}

// RecordIssueSyncSuccess completes the exact claimed run and advances cursor.
func (s *Store) RecordIssueSyncSuccess(
	ctx context.Context,
	params db.IssueSyncSuccessParams,
) (db.IssueSyncStatus, error) {
	var status db.IssueSyncStatus
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if _, err := scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.id=$1`, params.BindingID)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE issue_sync_status SET
 sync_started_at=NULL, last_success_at=$1, last_error_at=NULL, last_error=NULL,
 last_created=$2, last_updated=$3, last_unchanged=$4, last_comments=$5
WHERE binding_id=$6 AND sync_started_at=$7`,
			formatStoredTime(params.At), params.LastCreated, params.LastUpdated,
			params.LastUnchanged, params.LastComments, params.BindingID, formatStoredTime(params.StartedAt))
		if err != nil {
			return mapSQLError(err, nil)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return db.ErrIssueSyncAlreadyRunning
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issue_sync_bindings
SET last_cursor_at=$1, updated_at=$2 WHERE id=$3`,
			formatStoredTime(params.CursorAt), nowStoredTimestamp(), params.BindingID,
		); err != nil {
			return mapSQLError(err, nil)
		}
		status, err = scanIssueSyncStatus(tx.QueryRowContext(ctx,
			issueSyncStatusSelect+` WHERE binding_id=$1`, params.BindingID))
		return err
	})
	return status, err
}

// RecordIssueSyncError completes the exact claimed run with its latest error.
func (s *Store) RecordIssueSyncError(
	ctx context.Context,
	params db.IssueSyncErrorParams,
) (db.IssueSyncStatus, error) {
	var status db.IssueSyncStatus
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		if _, err := scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.id=$1`, params.BindingID)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE issue_sync_status SET
 sync_started_at=NULL, last_error_at=$1, last_error=$2
WHERE binding_id=$3 AND sync_started_at=$4`,
			formatStoredTime(params.At), params.Error, params.BindingID, formatStoredTime(params.StartedAt))
		if err != nil {
			return mapSQLError(err, nil)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return db.ErrIssueSyncAlreadyRunning
		}
		status, err = scanIssueSyncStatus(tx.QueryRowContext(ctx,
			issueSyncStatusSelect+` WHERE binding_id=$1`, params.BindingID))
		return err
	})
	return status, err
}

// RefreshIssueSyncBinding updates only provider-owned display and config data.
func (s *Store) RefreshIssueSyncBinding(
	ctx context.Context,
	params db.IssueSyncBindingUpdateParams,
) (db.IssueSyncBinding, error) {
	if strings.TrimSpace(params.DisplayName) == "" || !json.Valid(params.Config) {
		return db.IssueSyncBinding{}, fmt.Errorf(
			"%w: issue sync binding requires display name and valid config JSON", db.ErrImportValidation,
		)
	}
	var binding db.IssueSyncBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE issue_sync_bindings SET
display_name=$1, config_json=$2, updated_at=$3 WHERE id=$4`,
			params.DisplayName, string(params.Config), nowStoredTimestamp(), params.BindingID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return db.ErrNotFound
		}
		binding, err = scanIssueSyncBinding(tx.QueryRowContext(ctx,
			issueSyncBindingSelect+` WHERE b.id=$1`, params.BindingID))
		return err
	})
	return binding, err
}

func validateIssueSyncBindingParams(params db.UpsertIssueSyncBindingParams) error {
	if params.ProjectID <= 0 || strings.TrimSpace(params.Provider) == "" ||
		strings.TrimSpace(params.SourceKey) == "" || strings.TrimSpace(params.RemoteID) == "" ||
		strings.TrimSpace(params.DisplayName) == "" || params.IntervalSeconds <= 0 || !json.Valid(params.Config) {
		return fmt.Errorf("%w: invalid issue sync binding", db.ErrImportValidation)
	}
	return nil
}

func rejectFederationSpokeIssueSyncProject(ctx context.Context, query rowQueryer, projectID int64) error {
	var found int64
	err := query.QueryRowContext(ctx, `SELECT project_id FROM federation_bindings
WHERE project_id=$1 AND role=$2 AND enabled=1`, projectID, string(db.FederationRoleSpoke)).Scan(&found)
	if err == nil {
		return db.ErrIssueSyncFederationBinding
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("check issue sync federation binding: %w", mapSQLError(err, nil))
}

func scanIssueSyncBinding(row rowScanner) (db.IssueSyncBinding, error) {
	var binding db.IssueSyncBinding
	var config, createdAt, updatedAt string
	var enabled int
	var lastCursorAt sql.NullString
	err := row.Scan(
		&binding.ID, &binding.ProjectID, &binding.Provider, &binding.SourceKey,
		&binding.RemoteID, &binding.DisplayName, &config, &enabled, &binding.IntervalSeconds,
		&lastCursorAt, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueSyncBinding{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueSyncBinding{}, fmt.Errorf("scan issue sync binding: %w", mapSQLError(err, nil))
	}
	binding.Config = json.RawMessage(config)
	binding.Enabled = enabled == 1
	binding.CreatedAt, err = parseStoredTime(createdAt)
	if err != nil {
		return db.IssueSyncBinding{}, fmt.Errorf("parse issue sync created_at: %w", err)
	}
	binding.UpdatedAt, err = parseStoredTime(updatedAt)
	if err != nil {
		return db.IssueSyncBinding{}, fmt.Errorf("parse issue sync updated_at: %w", err)
	}
	if lastCursorAt.Valid {
		value, err := parseStoredTime(lastCursorAt.String)
		if err != nil {
			return db.IssueSyncBinding{}, fmt.Errorf("parse issue sync last_cursor_at: %w", err)
		}
		binding.LastCursorAt = &value
	}
	return binding, nil
}

func scanIssueSyncStatus(row rowScanner) (db.IssueSyncStatus, error) {
	var status db.IssueSyncStatus
	var syncStarted, lastAttempt, lastSuccess, lastErrorAt sql.NullString
	var lastError sql.NullString
	err := row.Scan(
		&status.BindingID, &status.ProjectID, &syncStarted, &lastAttempt,
		&lastSuccess, &lastErrorAt, &lastError, &status.LastCreated,
		&status.LastUpdated, &status.LastUnchanged, &status.LastComments,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return db.IssueSyncStatus{}, db.ErrNotFound
	}
	if err != nil {
		return db.IssueSyncStatus{}, fmt.Errorf("scan issue sync status: %w", mapSQLError(err, nil))
	}
	status.SyncStartedAt, err = parseNullableStoredTime(syncStarted)
	if err != nil {
		return db.IssueSyncStatus{}, fmt.Errorf("parse issue sync started_at: %w", err)
	}
	status.LastAttemptAt, err = parseNullableStoredTime(lastAttempt)
	if err != nil {
		return db.IssueSyncStatus{}, fmt.Errorf("parse issue sync last_attempt_at: %w", err)
	}
	status.LastSuccessAt, err = parseNullableStoredTime(lastSuccess)
	if err != nil {
		return db.IssueSyncStatus{}, fmt.Errorf("parse issue sync last_success_at: %w", err)
	}
	status.LastErrorAt, err = parseNullableStoredTime(lastErrorAt)
	if err != nil {
		return db.IssueSyncStatus{}, fmt.Errorf("parse issue sync last_error_at: %w", err)
	}
	if lastError.Valid {
		status.LastError = lastError.String
	}
	return status, nil
}

func parseNullableStoredTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseStoredTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

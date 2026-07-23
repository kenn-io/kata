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

const federationBindingSelect = `SELECT project_id, role, hub_url, hub_project_id,
       hub_project_uid, replay_horizon_event_id, pull_cursor_event_id, push_enabled,
       push_cursor_event_id, bound_actor, allow_insecure, enabled, created_at,
       updated_at, last_sync_at FROM federation_bindings`

const federationSyncStatusSelect = `SELECT project_id, last_pull_started_at,
       last_pull_success_at, last_push_started_at, last_push_success_at,
       last_error_at, last_error, last_reset_at FROM federation_sync_status`

const federationEnrollmentSelect = `SELECT id, token_hash, spoke_instance_uid,
       project_id, capabilities, bound_actor, allow_adoption_snapshot_authors,
       adoption_baseline_open, adoption_baseline_next_source_event_id,
       adoption_baseline_end_source_event_id, created_at, updated_at, revoked_at
  FROM federation_enrollments`

// ListFederationBindings returns every configured binding in project order.
func (s *Store) ListFederationBindings(ctx context.Context) ([]db.FederationBinding, error) {
	rows, err := s.QueryContext(ctx, federationBindingSelect+` ORDER BY project_id ASC`)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := []db.FederationBinding{}
	for rows.Next() {
		binding, err := scanFederationBinding(rows)
		if err != nil {
			return nil, err
		}
		output = append(output, binding)
	}
	return output, mapSQLError(rows.Err(), nil)
}

// FederationBindingByProject returns one project's binding.
func (s *Store) FederationBindingByProject(ctx context.Context, projectID int64) (db.FederationBinding, error) {
	return scanFederationBinding(s.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, projectID))
}

// UpsertFederationBinding creates or replaces one local binding.
func (s *Store) UpsertFederationBinding(ctx context.Context, input db.FederationBinding) (db.FederationBinding, error) {
	actor := strings.TrimSpace(input.Actor)
	if input.Role == db.FederationRoleSpoke && input.PushEnabled && actor == "" {
		return db.FederationBinding{}, fmt.Errorf("push-enabled federation spoke binding requires actor")
	}
	var output db.FederationBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.FederationBinding{}
		if _, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1 FOR UPDATE`, input.ProjectID)); err != nil {
			return err
		}
		if input.Role == db.FederationRoleSpoke {
			var issueSyncProjectID int64
			err := tx.QueryRowContext(ctx, `SELECT project_id FROM issue_sync_bindings
WHERE project_id=$1 AND enabled=1`, input.ProjectID).Scan(&issueSyncProjectID)
			if err == nil {
				return db.ErrIssueSyncFederationBinding
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return mapSQLError(err, nil)
			}
		}
		previous, err := federationBindingTransitionState(ctx, tx, input.ProjectID)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO federation_bindings(
  project_id, role, hub_url, hub_project_id, hub_project_uid,
  replay_horizon_event_id, pull_cursor_event_id, push_enabled,
  push_cursor_event_id, bound_actor, allow_insecure, enabled
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT(project_id) DO UPDATE SET role=EXCLUDED.role, hub_url=EXCLUDED.hub_url,
hub_project_id=EXCLUDED.hub_project_id, hub_project_uid=EXCLUDED.hub_project_uid,
replay_horizon_event_id=EXCLUDED.replay_horizon_event_id,
pull_cursor_event_id=EXCLUDED.pull_cursor_event_id, push_enabled=EXCLUDED.push_enabled,
push_cursor_event_id=EXCLUDED.push_cursor_event_id, bound_actor=EXCLUDED.bound_actor,
allow_insecure=EXCLUDED.allow_insecure, enabled=EXCLUDED.enabled,
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`,
			input.ProjectID, string(input.Role), input.HubURL, input.HubProjectID,
			input.HubProjectUID, input.ReplayHorizonEventID, input.PullCursorEventID,
			boolNumber(input.PushEnabled), input.PushCursorEventID, actor,
			boolNumber(input.AllowInsecure), boolNumber(input.Enabled))
		if err != nil {
			return mapSQLError(err, nil)
		}
		output, err = scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1`, input.ProjectID))
		if err != nil {
			return err
		}
		return reconcileFederationBindingTransitionLinks(ctx, tx, previous, output)
	})
	return output, err
}

// AdvanceFederationPullCursor records the highest consumed hub event id.
func (s *Store) AdvanceFederationPullCursor(ctx context.Context, projectID, nextCursor int64) error {
	return s.updateFederationCursor(ctx, projectID, nextCursor, false)
}

// AdvanceFederationPushCursor advances the acknowledged local event id monotonically.
func (s *Store) AdvanceFederationPushCursor(ctx context.Context, projectID, nextCursor int64) error {
	return s.updateFederationCursor(ctx, projectID, nextCursor, true)
}

func (s *Store) updateFederationCursor(ctx context.Context, projectID, nextCursor int64, push bool) error {
	column := "pull_cursor_event_id"
	if push {
		column = "push_cursor_event_id"
	}
	expression := `GREATEST(` + column + `,$1)`
	//nolint:gosec // column and expression are selected from the fixed cases above.
	query := `UPDATE federation_bindings SET ` + column + `=` + expression + `,
last_sync_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$2`
	result, err := s.ExecContext(ctx, query, nextCursor, projectID)
	if err != nil {
		return mapSQLError(err, nil)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return db.ErrNotFound
	}
	return nil
}

// EnableFederationPush marks an existing binding push-enabled without moving its cursor backward.
func (s *Store) EnableFederationPush(ctx context.Context, projectID int64, cursor int64) (db.FederationBinding, error) {
	var output db.FederationBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.FederationBinding{}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1 FOR UPDATE`, projectID))
		if err != nil {
			return err
		}
		if strings.TrimSpace(binding.Actor) == "" {
			return fmt.Errorf("enable federation push: bound actor is required")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET push_enabled=1,
push_cursor_event_id=GREATEST(push_cursor_event_id,$1),
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$2`, cursor, projectID); err != nil {
			return mapSQLError(err, nil)
		}
		output, err = scanFederationBinding(tx.QueryRowContext(ctx,
			federationBindingSelect+` WHERE project_id=$1`, projectID))
		return err
	})
	return output, err
}

// FederationSyncStatusByProject returns one binding's persisted worker status.
func (s *Store) FederationSyncStatusByProject(ctx context.Context, projectID int64) (db.FederationSyncStatus, error) {
	return scanFederationSyncStatus(s.QueryRowContext(ctx,
		federationSyncStatusSelect+` WHERE project_id=$1`, projectID))
}

// RecordFederationSyncPullStarted records the start of a pull attempt.
func (s *Store) RecordFederationSyncPullStarted(ctx context.Context, projectID int64, at time.Time) error {
	return s.upsertFederationSyncTime(ctx, projectID, "last_pull_started_at", at)
}

// RecordFederationSyncPullSuccess records a successful pull.
func (s *Store) RecordFederationSyncPullSuccess(ctx context.Context, projectID int64, at time.Time) error {
	return s.upsertFederationSyncTime(ctx, projectID, "last_pull_success_at", at)
}

// RecordFederationSyncPushStarted records the start of a push attempt.
func (s *Store) RecordFederationSyncPushStarted(ctx context.Context, projectID int64, at time.Time) error {
	return s.upsertFederationSyncTime(ctx, projectID, "last_push_started_at", at)
}

// RecordFederationSyncPushSuccess records a successful push.
func (s *Store) RecordFederationSyncPushSuccess(ctx context.Context, projectID int64, at time.Time) error {
	return s.upsertFederationSyncTime(ctx, projectID, "last_push_success_at", at)
}

// RecordFederationSyncReset records completion of a projection reset.
func (s *Store) RecordFederationSyncReset(ctx context.Context, projectID int64, at time.Time) error {
	return s.upsertFederationSyncTime(ctx, projectID, "last_reset_at", at)
}

func (s *Store) upsertFederationSyncTime(ctx context.Context, projectID int64, column string, at time.Time) error {
	switch column {
	case "last_pull_started_at", "last_pull_success_at", "last_push_started_at", "last_push_success_at", "last_reset_at":
	default:
		return fmt.Errorf("unsupported federation sync status column %q", column)
	}
	//nolint:gosec // column is restricted to the fixed allowlist above.
	query := fmt.Sprintf(`INSERT INTO federation_sync_status(project_id,%s)
SELECT $1,$2 WHERE EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)
ON CONFLICT(project_id) DO UPDATE SET %s=EXCLUDED.%s`, column, column, column)
	_, err := s.ExecContext(ctx, query, projectID, formatStoredTime(at.UTC()))
	return mapSQLError(err, nil)
}

// RecordFederationSyncError stores the latest worker failure.
func (s *Store) RecordFederationSyncError(ctx context.Context, projectID int64, syncErr error, at time.Time) error {
	message := ""
	if syncErr != nil {
		message = syncErr.Error()
	}
	_, err := s.ExecContext(ctx, `INSERT INTO federation_sync_status(project_id,last_error_at,last_error)
SELECT $1,$2,$3 WHERE EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)
ON CONFLICT(project_id) DO UPDATE SET last_error_at=EXCLUDED.last_error_at,last_error=EXCLUDED.last_error`,
		projectID, formatStoredTime(at.UTC()), message)
	return mapSQLError(err, nil)
}

// ClearFederationSyncError clears the project-level worker error.
func (s *Store) ClearFederationSyncError(ctx context.Context, projectID int64) error {
	_, err := s.ExecContext(ctx, `INSERT INTO federation_sync_status(project_id,last_error_at,last_error)
SELECT $1,NULL,NULL WHERE EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)
ON CONFLICT(project_id) DO UPDATE SET last_error_at=NULL,last_error=NULL`, projectID)
	return mapSQLError(err, nil)
}

// CreateFederationEnrollment creates an active enrollment and returns its secret once.
func (s *Store) CreateFederationEnrollment(
	ctx context.Context,
	input db.CreateFederationEnrollmentParams,
) (db.CreatedFederationEnrollment, error) {
	prepared, err := db.PrepareFederationEnrollmentParams(input)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	var output db.CreatedFederationEnrollment
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.CreatedFederationEnrollment{}
		var err error
		output, err = createFederationEnrollmentTx(ctx, tx, prepared)
		return err
	})
	return output, err
}

// CreateProjectFederationEnrollment enables one hub project and creates its
// scoped enrollment in the same transaction.
func (s *Store) CreateProjectFederationEnrollment(
	ctx context.Context,
	input db.CreateFederationEnrollmentParams,
) (db.CreatedFederationEnrollment, error) {
	prepared, err := db.PrepareFederationEnrollmentParams(input)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	if prepared.Params.ProjectID == nil || *prepared.Params.ProjectID <= 0 {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("project-scoped federation enrollment requires project id")
	}
	var output db.CreatedFederationEnrollment
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.CreatedFederationEnrollment{}
		p := prepared.Params
		if _, err := s.enableProjectFederationTx(ctx, tx, *p.ProjectID, p.Actor); err != nil {
			return err
		}
		var err error
		output, err = createFederationEnrollmentTx(ctx, tx, prepared)
		return err
	})
	return output, err
}

// RotateFederationEnrollment replaces active grants for one spoke and project
// atomically with the caller-supplied credential.
func (s *Store) RotateFederationEnrollment(
	ctx context.Context,
	input db.CreateFederationEnrollmentParams,
) (db.CreatedFederationEnrollment, error) {
	if input.Token == "" {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("federation enrollment rotation requires replacement token")
	}
	prepared, err := db.PrepareFederationEnrollmentParams(input)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	if prepared.Params.ProjectID == nil || *prepared.Params.ProjectID <= 0 {
		return db.CreatedFederationEnrollment{}, fmt.Errorf(
			"federation enrollment rotation requires project id",
		)
	}
	var output db.CreatedFederationEnrollment
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.CreatedFederationEnrollment{}
		p := prepared.Params
		existing, err := scanFederationEnrollment(tx.QueryRowContext(ctx,
			federationEnrollmentSelect+` WHERE token_hash=$1`, db.FederationTokenHash(p.Token)))
		matched := err == nil
		if matched {
			if !db.FederationEnrollmentMatchesCreate(existing, p) {
				return db.ErrFederationEnrollmentTokenConflict
			}
		} else if !errors.Is(err, db.ErrNotFound) {
			return err
		} else if s.rotationStage != nil {
			if err := s.rotationStage(ctx); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx, `UPDATE federation_enrollments SET
revoked_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE revoked_at IS NULL AND spoke_instance_uid=$1 AND project_id=$2 AND token_hash<>$3`,
			p.SpokeInstanceUID, *p.ProjectID, db.FederationTokenHash(p.Token)); err != nil {
			return mapSQLError(err, nil)
		}
		if matched {
			output = db.CreatedFederationEnrollment{Enrollment: existing, Token: p.Token}
			return nil
		}
		output, err = createFederationEnrollmentTx(ctx, tx, prepared)
		return err
	})
	return output, err
}

func createFederationEnrollmentTx(
	ctx context.Context,
	tx *sql.Tx,
	prepared db.PreparedFederationEnrollment,
) (db.CreatedFederationEnrollment, error) {
	input := prepared.Params
	var projectID any
	if input.ProjectID != nil {
		projectID = *input.ProjectID
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO federation_enrollments(
  token_hash,spoke_instance_uid,project_id,capabilities,bound_actor,
  allow_adoption_snapshot_authors
) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(token_hash) DO NOTHING
RETURNING id`, db.FederationTokenHash(input.Token),
		input.SpokeInstanceUID, projectID, input.Capabilities, input.Actor,
		boolNumber(input.AllowAdoptionSnapshotAuthors)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if !prepared.ExplicitToken {
			return db.CreatedFederationEnrollment{}, fmt.Errorf("create federation enrollment: generated token collision")
		}
		enrollment, selectErr := scanFederationEnrollment(tx.QueryRowContext(ctx,
			federationEnrollmentSelect+` WHERE token_hash=$1`, db.FederationTokenHash(input.Token)))
		if selectErr != nil {
			return db.CreatedFederationEnrollment{}, selectErr
		}
		if !db.FederationEnrollmentMatchesCreate(enrollment, input) {
			return db.CreatedFederationEnrollment{}, db.ErrFederationEnrollmentTokenConflict
		}
		return db.CreatedFederationEnrollment{Enrollment: enrollment, Token: input.Token}, nil
	}
	if err != nil {
		return db.CreatedFederationEnrollment{}, mapSQLError(err, nil)
	}
	enrollment, err := federationEnrollmentByIDTx(ctx, tx, id, false)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	return db.CreatedFederationEnrollment{Enrollment: enrollment, Token: input.Token}, nil
}

// ListFederationEnrollments returns enrollment history in identity order.
func (s *Store) ListFederationEnrollments(ctx context.Context) ([]db.FederationEnrollment, error) {
	rows, err := s.QueryContext(ctx, federationEnrollmentSelect+` ORDER BY id ASC`)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := []db.FederationEnrollment{}
	for rows.Next() {
		enrollment, err := scanFederationEnrollment(rows)
		if err != nil {
			return nil, err
		}
		output = append(output, enrollment)
	}
	return output, mapSQLError(rows.Err(), nil)
}

// FindActiveFederationEnrollment returns the newest active enrollment whose
// public correlation fields exactly match the request.
func (s *Store) FindActiveFederationEnrollment(
	ctx context.Context,
	p db.ActiveFederationEnrollmentParams,
) (db.FederationEnrollment, error) {
	return scanFederationEnrollment(s.QueryRowContext(ctx, federationEnrollmentSelect+`
WHERE project_id=$1
  AND spoke_instance_uid=$2
  AND capabilities=$3
  AND bound_actor=$4
  AND allow_adoption_snapshot_authors=$5
  AND revoked_at IS NULL
ORDER BY id DESC
LIMIT 1`,
		p.ProjectID, p.SpokeInstanceUID, p.Capabilities, p.Actor,
		boolNumber(p.AllowAdoptionSnapshotAuthors)))
}

// RevokeFederationEnrollment marks an enrollment inactive without changing its first revocation time.
func (s *Store) RevokeFederationEnrollment(ctx context.Context, id int64) error {
	result, err := s.ExecContext(ctx, `UPDATE federation_enrollments SET
revoked_at=COALESCE(revoked_at,to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') WHERE id=$1`, id)
	if err != nil {
		return mapSQLError(err, nil)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return db.ErrNotFound
	}
	return nil
}

// AuthorizeFederationToken validates scope, capability, and the target hub binding.
func (s *Store) AuthorizeFederationToken(
	ctx context.Context,
	token string,
	projectID int64,
	capability string,
) (db.FederationEnrollment, error) {
	capability = strings.TrimSpace(capability)
	if token == "" || !db.IsSupportedFederationCapability(capability) {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	enrollment, err := scanFederationEnrollment(s.QueryRowContext(ctx, federationEnrollmentSelect+`
WHERE token_hash=$1 AND revoked_at IS NULL
  AND (',' || capabilities || ',') LIKE ('%,' || $2 || ',%')
  AND (project_id=$3 OR project_id IS NULL)
  AND EXISTS(SELECT 1 FROM federation_bindings b JOIN projects p ON p.id=b.project_id
             WHERE b.project_id=$3 AND p.deleted_at IS NULL AND b.role='hub' AND b.enabled=1)`,
		db.FederationTokenHash(token), capability, projectID))
	if err != nil {
		return db.FederationEnrollment{}, err
	}
	if enrollment.ProjectID == nil {
		enrollment.AllowAdoptionSnapshotAuthors = false
		enrollment.AdoptionBaselineOpen = false
		enrollment.AdoptionBaselineNextSourceEventID = 0
		enrollment.AdoptionBaselineEndSourceEventID = 0
	}
	return enrollment, nil
}

// FederationEnrollmentTransactionFence rechecks native credential authority
// in the same transaction as the protected mutation.
func (s *Store) FederationEnrollmentTransactionFence(
	admitted db.FederationEnrollment,
	projectID int64,
	capability string,
) db.TransactionFence {
	return func(ctx context.Context, transaction db.Transaction) error {
		current, err := scanFederationEnrollment(transaction.QueryRowContext(ctx,
			federationEnrollmentSelect+` WHERE id=$1 FOR SHARE`, admitted.ID))
		if err != nil {
			return err
		}
		if !db.FederationEnrollmentAuthorizationMatches(current, admitted, projectID, capability) {
			return db.ErrNotFound
		}
		var active int
		err = transaction.QueryRowContext(ctx, `
			SELECT binding.enabled
			FROM federation_bindings AS binding
			JOIN projects AS project ON project.id=binding.project_id
			WHERE binding.project_id=$1 AND binding.role='hub'
			  AND binding.enabled=1 AND project.deleted_at IS NULL
			FOR SHARE OF binding, project`, projectID).Scan(&active)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return err
		}
		if active != 1 {
			return db.ErrNotFound
		}
		return nil
	}
}

// CountActiveFederationEnrollments returns active project and wildcard enrollments.
func (s *Store) CountActiveFederationEnrollments(ctx context.Context, projectID int64) (int64, error) {
	var count int64
	err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_enrollments
WHERE revoked_at IS NULL AND (project_id=$1 OR project_id IS NULL)`, projectID).Scan(&count)
	return count, mapSQLError(err, nil)
}

func scanFederationBinding(row rowScanner) (db.FederationBinding, error) {
	var binding db.FederationBinding
	var role string
	var pushEnabled, allowInsecure, enabled int
	var createdAt, updatedAt string
	var lastSyncAt sql.NullString
	err := row.Scan(&binding.ProjectID, &role, &binding.HubURL, &binding.HubProjectID,
		&binding.HubProjectUID, &binding.ReplayHorizonEventID, &binding.PullCursorEventID,
		&pushEnabled, &binding.PushCursorEventID, &binding.Actor, &allowInsecure, &enabled,
		&createdAt, &updatedAt, &lastSyncAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.FederationBinding{}, db.ErrNotFound
	}
	if err != nil {
		return db.FederationBinding{}, mapSQLError(err, nil)
	}
	binding.Role = db.FederationRole(role)
	binding.PushEnabled = pushEnabled == 1
	binding.AllowInsecure = allowInsecure == 1
	binding.Enabled = enabled == 1
	if binding.CreatedAt, err = parseStoredTime(createdAt); err != nil {
		return db.FederationBinding{}, fmt.Errorf("parse federation binding created_at: %w", err)
	}
	if binding.UpdatedAt, err = parseStoredTime(updatedAt); err != nil {
		return db.FederationBinding{}, fmt.Errorf("parse federation binding updated_at: %w", err)
	}
	if binding.LastSyncAt, err = parseNullableStoredTime(lastSyncAt); err != nil {
		return db.FederationBinding{}, fmt.Errorf("parse federation binding last_sync_at: %w", err)
	}
	return binding, nil
}

func scanFederationSyncStatus(row rowScanner) (db.FederationSyncStatus, error) {
	var status db.FederationSyncStatus
	var pullStarted, pullSuccess, pushStarted, pushSuccess sql.NullString
	var lastErrorAt, lastError, lastResetAt sql.NullString
	err := row.Scan(&status.ProjectID, &pullStarted, &pullSuccess, &pushStarted,
		&pushSuccess, &lastErrorAt, &lastError, &lastResetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.FederationSyncStatus{}, db.ErrNotFound
	}
	if err != nil {
		return db.FederationSyncStatus{}, mapSQLError(err, nil)
	}
	var parseErr error
	if status.LastPullStartedAt, parseErr = parseNullableStoredTime(pullStarted); parseErr != nil {
		return db.FederationSyncStatus{}, parseErr
	}
	if status.LastPullSuccessAt, parseErr = parseNullableStoredTime(pullSuccess); parseErr != nil {
		return db.FederationSyncStatus{}, parseErr
	}
	if status.LastPushStartedAt, parseErr = parseNullableStoredTime(pushStarted); parseErr != nil {
		return db.FederationSyncStatus{}, parseErr
	}
	if status.LastPushSuccessAt, parseErr = parseNullableStoredTime(pushSuccess); parseErr != nil {
		return db.FederationSyncStatus{}, parseErr
	}
	if status.LastErrorAt, parseErr = parseNullableStoredTime(lastErrorAt); parseErr != nil {
		return db.FederationSyncStatus{}, parseErr
	}
	if status.LastResetAt, parseErr = parseNullableStoredTime(lastResetAt); parseErr != nil {
		return db.FederationSyncStatus{}, parseErr
	}
	if lastError.Valid {
		status.LastError = &lastError.String
	}
	return status, nil
}

func federationEnrollmentByIDTx(
	ctx context.Context,
	queryer claimQueryer,
	id int64,
	lock bool,
) (db.FederationEnrollment, error) {
	query := federationEnrollmentSelect + ` WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanFederationEnrollment(queryer.QueryRowContext(ctx, query, id))
}

func scanFederationEnrollment(row rowScanner) (db.FederationEnrollment, error) {
	var enrollment db.FederationEnrollment
	var projectID sql.NullInt64
	var allowAuthors, baselineOpen int
	var createdAt, updatedAt string
	var revokedAt sql.NullString
	err := row.Scan(&enrollment.ID, &enrollment.TokenHash, &enrollment.SpokeInstanceUID,
		&projectID, &enrollment.Capabilities, &enrollment.Actor, &allowAuthors,
		&baselineOpen, &enrollment.AdoptionBaselineNextSourceEventID,
		&enrollment.AdoptionBaselineEndSourceEventID, &createdAt, &updatedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	if err != nil {
		return db.FederationEnrollment{}, mapSQLError(err, nil)
	}
	if projectID.Valid {
		enrollment.ProjectID = &projectID.Int64
	}
	enrollment.AllowAdoptionSnapshotAuthors = allowAuthors == 1
	enrollment.AdoptionBaselineOpen = baselineOpen == 1
	if enrollment.CreatedAt, err = parseStoredTime(createdAt); err != nil {
		return db.FederationEnrollment{}, err
	}
	if enrollment.UpdatedAt, err = parseStoredTime(updatedAt); err != nil {
		return db.FederationEnrollment{}, err
	}
	if enrollment.RevokedAt, err = parseNullableStoredTime(revokedAt); err != nil {
		return db.FederationEnrollment{}, err
	}
	return enrollment, nil
}

func boolNumber(value bool) int {
	if value {
		return 1
	}
	return 0
}

func encodeStringList(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

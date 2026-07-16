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

const federationQuarantineSelect = `SELECT id, project_id, direction,
       first_event_id, last_event_id, event_uids, error, created_at,
       skipped_at, skipped_by, skip_reason FROM federation_quarantine`

// RecordFederationQuarantine creates or returns the active poisoned batch.
func (s *Store) RecordFederationQuarantine(
	ctx context.Context,
	input db.RecordFederationQuarantineParams,
) (db.FederationQuarantine, error) {
	if err := db.ValidateFederationQuarantine(input); err != nil {
		return db.FederationQuarantine{}, err
	}
	eventUIDs, err := encodeStringList(input.EventUIDs)
	if err != nil {
		return db.FederationQuarantine{}, fmt.Errorf("encode federation quarantine event uids: %w", err)
	}
	var output db.FederationQuarantine
	err = s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.FederationQuarantine{}
		_, err := tx.ExecContext(ctx, `INSERT INTO federation_quarantine(
  project_id,direction,first_event_id,last_event_id,event_uids,error,created_at
) SELECT $1,$2,$3,$4,$5,$6,$7
WHERE EXISTS(SELECT 1 FROM federation_bindings WHERE project_id=$1)
ON CONFLICT(project_id,direction) WHERE skipped_at IS NULL DO NOTHING`, input.ProjectID,
			string(input.Direction), input.FirstEventID, input.LastEventID, eventUIDs,
			input.Error, formatStoredTime(input.CreatedAt.UTC()))
		if err != nil {
			return mapSQLError(err, nil)
		}
		output, err = scanFederationQuarantine(tx.QueryRowContext(ctx,
			federationQuarantineSelect+` WHERE project_id=$1 AND direction=$2 AND skipped_at IS NULL`,
			input.ProjectID, string(input.Direction)))
		if errors.Is(err, db.ErrNotFound) {
			output = db.FederationQuarantine{}
			return nil
		}
		return err
	})
	return output, err
}

// ActiveFederationQuarantine returns one unresolved quarantine.
func (s *Store) ActiveFederationQuarantine(
	ctx context.Context,
	projectID int64,
	direction db.FederationQuarantineDirection,
) (db.FederationQuarantine, error) {
	return scanFederationQuarantine(s.QueryRowContext(ctx, federationQuarantineSelect+
		` WHERE project_id=$1 AND direction=$2 AND skipped_at IS NULL`, projectID, string(direction)))
}

// ActiveFederationQuarantinesByProject returns all unresolved quarantines for a project.
func (s *Store) ActiveFederationQuarantinesByProject(ctx context.Context, projectID int64) ([]db.FederationQuarantine, error) {
	rows, err := s.QueryContext(ctx, federationQuarantineSelect+
		` WHERE project_id=$1 AND skipped_at IS NULL ORDER BY id ASC`, projectID)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	defer func() { _ = rows.Close() }()
	output := []db.FederationQuarantine{}
	for rows.Next() {
		quarantine, err := scanFederationQuarantine(rows)
		if err != nil {
			return nil, err
		}
		output = append(output, quarantine)
	}
	return output, mapSQLError(rows.Err(), nil)
}

// SkipFederationQuarantine resolves a push quarantine and advances its cursor.
func (s *Store) SkipFederationQuarantine(
	ctx context.Context,
	input db.SkipFederationQuarantineParams,
) (db.FederationQuarantine, error) {
	actor := strings.TrimSpace(input.Actor)
	if actor == "" {
		return db.FederationQuarantine{}, fmt.Errorf("skip federation quarantine: actor is required")
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var output db.FederationQuarantine
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.FederationQuarantine{}
		quarantine, err := scanFederationQuarantine(tx.QueryRowContext(ctx,
			federationQuarantineSelect+` WHERE id=$1 AND project_id=$2 AND skipped_at IS NULL FOR UPDATE`,
			input.ID, input.ProjectID))
		if err != nil {
			return err
		}
		if quarantine.Direction != db.FederationQuarantineDirectionPush {
			return fmt.Errorf("skip federation quarantine: unsupported direction %q", quarantine.Direction)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_bindings SET
push_cursor_event_id=GREATEST(push_cursor_event_id,$1),
last_sync_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE project_id=$2`, quarantine.LastEventID, quarantine.ProjectID); err != nil {
			return mapSQLError(err, nil)
		}
		_, err = tx.ExecContext(ctx, `UPDATE federation_quarantine SET skipped_at=$1,
skipped_by=$2,skip_reason=$3 WHERE id=$4 AND project_id=$5 AND skipped_at IS NULL`,
			formatStoredTime(now.UTC()), actor, strings.TrimSpace(input.Reason), input.ID, input.ProjectID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		output, err = scanFederationQuarantine(tx.QueryRowContext(ctx,
			federationQuarantineSelect+` WHERE id=$1 AND project_id=$2`, input.ID, input.ProjectID))
		return err
	})
	return output, err
}

// RetryFederationQuarantine resolves a push quarantine without advancing its cursor.
func (s *Store) RetryFederationQuarantine(
	ctx context.Context,
	input db.RetryFederationQuarantineParams,
) (db.FederationQuarantine, error) {
	actor := strings.TrimSpace(input.Actor)
	if actor == "" {
		return db.FederationQuarantine{}, fmt.Errorf("retry federation quarantine: actor is required")
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var output db.FederationQuarantine
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		output = db.FederationQuarantine{}
		quarantine, err := scanFederationQuarantine(tx.QueryRowContext(ctx,
			federationQuarantineSelect+` WHERE id=$1 AND project_id=$2 AND skipped_at IS NULL FOR UPDATE`,
			input.ID, input.ProjectID))
		if err != nil {
			return err
		}
		if quarantine.Direction != db.FederationQuarantineDirectionPush {
			return fmt.Errorf("%w: %s", db.ErrFederationQuarantineRetryUnsupportedDirection, quarantine.Direction)
		}
		reason := strings.TrimSpace(input.Reason)
		if reason == "" {
			reason = "operator requested retry"
		}
		_, err = tx.ExecContext(ctx, `UPDATE federation_quarantine SET skipped_at=$1,
skipped_by=$2,skip_reason=$3 WHERE id=$4 AND project_id=$5 AND skipped_at IS NULL`,
			formatStoredTime(now.UTC()), actor, "retry: "+reason, input.ID, input.ProjectID)
		if err != nil {
			return mapSQLError(err, nil)
		}
		output, err = scanFederationQuarantine(tx.QueryRowContext(ctx,
			federationQuarantineSelect+` WHERE id=$1 AND project_id=$2`, input.ID, input.ProjectID))
		return err
	})
	return output, err
}

func scanFederationQuarantine(row rowScanner) (db.FederationQuarantine, error) {
	var quarantine db.FederationQuarantine
	var direction, eventUIDs, createdAt string
	var skippedAt, skippedBy, skipReason sql.NullString
	err := row.Scan(&quarantine.ID, &quarantine.ProjectID, &direction,
		&quarantine.FirstEventID, &quarantine.LastEventID, &eventUIDs,
		&quarantine.Error, &createdAt, &skippedAt, &skippedBy, &skipReason)
	if errors.Is(err, sql.ErrNoRows) {
		return db.FederationQuarantine{}, db.ErrNotFound
	}
	if err != nil {
		return db.FederationQuarantine{}, mapSQLError(err, nil)
	}
	quarantine.Direction = db.FederationQuarantineDirection(direction)
	if err := json.Unmarshal([]byte(eventUIDs), &quarantine.EventUIDs); err != nil {
		return db.FederationQuarantine{}, fmt.Errorf("decode federation quarantine event uids: %w", err)
	}
	if quarantine.EventUIDs == nil {
		quarantine.EventUIDs = []string{}
	}
	if quarantine.CreatedAt, err = parseStoredTime(createdAt); err != nil {
		return db.FederationQuarantine{}, err
	}
	if quarantine.SkippedAt, err = parseNullableStoredTime(skippedAt); err != nil {
		return db.FederationQuarantine{}, err
	}
	if skippedBy.Valid {
		quarantine.SkippedBy = &skippedBy.String
	}
	if skipReason.Valid {
		quarantine.SkipReason = &skipReason.String
	}
	return quarantine, nil
}

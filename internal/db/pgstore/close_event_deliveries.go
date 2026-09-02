package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

func insertCloseEventDeliveryTx(
	ctx context.Context,
	tx *sql.Tx,
	params db.CloseIssueParams,
	projectID int64,
	issueUID string,
	events []db.Event,
	at string,
) error {
	if params.IdempotencyKey == "" {
		return nil
	}
	if params.IdempotencyFingerprint == "" || len(events) == 0 {
		return errors.New("record close event delivery: fingerprint and events are required")
	}
	eventUIDs := make([]string, len(events))
	for i := range events {
		eventUIDs[i] = events[i].UID
	}
	encoded, err := json.Marshal(eventUIDs)
	if err != nil {
		return fmt.Errorf("encode close event delivery: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO close_event_deliveries
      (project_id, idempotency_key, issue_uid, fingerprint, event_uids, created_at, updated_at)
      VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		projectID, params.IdempotencyKey, issueUID,
		params.IdempotencyFingerprint, string(encoded), at)
	if err != nil {
		return fmt.Errorf("record close event delivery: %w", mapSQLError(err, nil))
	}
	return nil
}

// ClaimCloseEventDelivery gives one publisher a bounded claim on a keyed
// close's ordered event batch.
func (s *Store) ClaimCloseEventDelivery(
	ctx context.Context, params db.ClaimCloseEventDeliveryParams,
) (db.CloseEventDeliveryClaim, error) {
	if err := validateCloseEventDeliveryClaim(params); err != nil {
		return db.CloseEventDeliveryClaim{}, err
	}
	var claim db.CloseEventDeliveryClaim
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE close_event_deliveries
 SET state = 'delivering', claim_token = $1, claim_expires_at = $2, updated_at = $3
WHERE project_id = $4 AND idempotency_key = $5 AND fingerprint = $6
  AND state <> 'delivered'
  AND (state = 'pending' OR claim_expires_at <= $3 OR claim_token = $1)`,
			params.ClaimToken, formatStoredTime(params.ClaimExpiresAt), formatStoredTime(params.ClaimedAt),
			params.ProjectID, params.IdempotencyKey, params.Fingerprint)
		if err != nil {
			return mapSQLError(err, nil)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		var fingerprint, encodedUIDs, state string
		err = tx.QueryRowContext(ctx, `SELECT fingerprint, event_uids, state
 FROM close_event_deliveries WHERE project_id = $1 AND idempotency_key = $2`,
			params.ProjectID, params.IdempotencyKey).Scan(&fingerprint, &encodedUIDs, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		if fingerprint != params.Fingerprint {
			return db.ErrCloseEventDeliveryFingerprintMismatch
		}
		if err := json.Unmarshal([]byte(encodedUIDs), &claim.EventUIDs); err != nil {
			return fmt.Errorf("decode close event delivery: %w", err)
		}
		claim.Acquired = affected > 0
		claim.Delivered = state == "delivered"
		return nil
	})
	return claim, err
}

func validateCloseEventDeliveryClaim(params db.ClaimCloseEventDeliveryParams) error {
	if params.ProjectID <= 0 || params.IdempotencyKey == "" ||
		params.Fingerprint == "" || strings.TrimSpace(params.ClaimToken) == "" ||
		params.ClaimedAt.IsZero() || !params.ClaimExpiresAt.After(params.ClaimedAt) {
		return errors.New("claim close event delivery: invalid parameters")
	}
	return nil
}

func (s *Store) ReleaseCloseEventDeliveryClaim(
	ctx context.Context, params db.CloseEventDeliveryClaimUpdateParams,
) error {
	return s.updateCloseEventDeliveryClaim(ctx, params, false)
}

func (s *Store) CompleteCloseEventDelivery(
	ctx context.Context, params db.CloseEventDeliveryClaimUpdateParams,
) error {
	return s.updateCloseEventDeliveryClaim(ctx, params, true)
}

func (s *Store) updateCloseEventDeliveryClaim(
	ctx context.Context, params db.CloseEventDeliveryClaimUpdateParams, complete bool,
) error {
	if params.ProjectID <= 0 || params.IdempotencyKey == "" ||
		params.Fingerprint == "" || strings.TrimSpace(params.ClaimToken) == "" || params.At.IsZero() {
		return errors.New("update close event delivery claim: invalid parameters")
	}
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		state := "pending"
		deliveredAt := any(nil)
		if complete {
			state = "delivered"
			deliveredAt = formatStoredTime(params.At)
		}
		result, err := tx.ExecContext(ctx, `UPDATE close_event_deliveries
 SET state = $1, claim_token = NULL, claim_expires_at = NULL,
     delivered_at = $2, updated_at = $3
WHERE project_id = $4 AND idempotency_key = $5 AND fingerprint = $6
  AND state = 'delivering' AND claim_token = $7`,
			state, deliveredAt, formatStoredTime(params.At), params.ProjectID,
			params.IdempotencyKey, params.Fingerprint, params.ClaimToken)
		if err != nil {
			return mapSQLError(err, nil)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected > 0 {
			return nil
		}
		var fingerprint, currentState string
		err = tx.QueryRowContext(ctx, `SELECT fingerprint, state
 FROM close_event_deliveries WHERE project_id = $1 AND idempotency_key = $2`,
			params.ProjectID, params.IdempotencyKey).Scan(&fingerprint, &currentState)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		if fingerprint != params.Fingerprint {
			return db.ErrCloseEventDeliveryFingerprintMismatch
		}
		if complete && currentState == "delivered" {
			return nil
		}
		if !complete && currentState == "pending" {
			return nil
		}
		return db.ErrCloseEventDeliveryClaimLost
	})
}

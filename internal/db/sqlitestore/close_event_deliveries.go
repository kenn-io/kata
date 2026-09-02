package sqlitestore

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
      VALUES (?, ?, ?, ?, ?, ?, ?)`,
		projectID, params.IdempotencyKey, issueUID,
		params.IdempotencyFingerprint, string(encoded), at, at)
	if err != nil {
		return fmt.Errorf("record close event delivery: %w", err)
	}
	return nil
}

// ClaimCloseEventDelivery gives one publisher a bounded claim on a keyed
// close's ordered event batch. An expired claim may be recovered by another
// daemon; a delivered batch is returned without being claimed again.
func (d *Store) ClaimCloseEventDelivery(
	ctx context.Context, params db.ClaimCloseEventDeliveryParams,
) (db.CloseEventDeliveryClaim, error) {
	var claim db.CloseEventDeliveryClaim
	err := d.RetryTransient(ctx, func() error {
		var err error
		claim, err = d.claimCloseEventDelivery(ctx, params)
		return err
	})
	return claim, err
}

func (d *Store) claimCloseEventDelivery(
	ctx context.Context, params db.ClaimCloseEventDeliveryParams,
) (db.CloseEventDeliveryClaim, error) {
	if err := validateCloseEventDeliveryClaim(params); err != nil {
		return db.CloseEventDeliveryClaim{}, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.CloseEventDeliveryClaim{}, fmt.Errorf("begin close event delivery claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `UPDATE close_event_deliveries
       SET state = 'delivering', claim_token = ?, claim_expires_at = ?, updated_at = ?
	     WHERE project_id = ? AND idempotency_key = ? AND fingerprint = ?
	       AND state <> 'delivered'
	       AND (state = 'pending' OR claim_expires_at <= ? OR claim_token = ?)`,
		params.ClaimToken,
		params.ClaimExpiresAt.UTC().Format(sqliteTimeFormat),
		params.ClaimedAt.UTC().Format(sqliteTimeFormat),
		params.ProjectID, params.IdempotencyKey, params.Fingerprint,
		params.ClaimedAt.UTC().Format(sqliteTimeFormat), params.ClaimToken)
	if err != nil {
		return db.CloseEventDeliveryClaim{}, fmt.Errorf("claim close event delivery: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return db.CloseEventDeliveryClaim{}, fmt.Errorf("read close event delivery claim result: %w", err)
	}

	var fingerprint, encodedUIDs, state string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint, event_uids, state
      FROM close_event_deliveries WHERE project_id = ? AND idempotency_key = ?`,
		params.ProjectID, params.IdempotencyKey).Scan(&fingerprint, &encodedUIDs, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return db.CloseEventDeliveryClaim{}, db.ErrNotFound
	}
	if err != nil {
		return db.CloseEventDeliveryClaim{}, fmt.Errorf("read close event delivery: %w", err)
	}
	if fingerprint != params.Fingerprint {
		return db.CloseEventDeliveryClaim{}, db.ErrCloseEventDeliveryFingerprintMismatch
	}
	var eventUIDs []string
	if err := json.Unmarshal([]byte(encodedUIDs), &eventUIDs); err != nil {
		return db.CloseEventDeliveryClaim{}, fmt.Errorf("decode close event delivery: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return db.CloseEventDeliveryClaim{}, fmt.Errorf("commit close event delivery claim: %w", err)
	}
	return db.CloseEventDeliveryClaim{
		EventUIDs: eventUIDs,
		Acquired:  affected > 0,
		Delivered: state == "delivered",
	}, nil
}

func validateCloseEventDeliveryClaim(params db.ClaimCloseEventDeliveryParams) error {
	if params.ProjectID <= 0 || params.IdempotencyKey == "" ||
		params.Fingerprint == "" || strings.TrimSpace(params.ClaimToken) == "" ||
		params.ClaimedAt.IsZero() || !params.ClaimExpiresAt.After(params.ClaimedAt) {
		return errors.New("claim close event delivery: invalid parameters")
	}
	return nil
}

// ReleaseCloseEventDeliveryClaim returns an unbroadcast batch to pending.
func (d *Store) ReleaseCloseEventDeliveryClaim(
	ctx context.Context, params db.CloseEventDeliveryClaimUpdateParams,
) error {
	return d.updateCloseEventDeliveryClaim(ctx, params, false)
}

// CompleteCloseEventDelivery marks a broadcast batch delivered. Repeating a
// completion after an ambiguous commit is safe.
func (d *Store) CompleteCloseEventDelivery(
	ctx context.Context, params db.CloseEventDeliveryClaimUpdateParams,
) error {
	return d.updateCloseEventDeliveryClaim(ctx, params, true)
}

func (d *Store) updateCloseEventDeliveryClaim(
	ctx context.Context, params db.CloseEventDeliveryClaimUpdateParams, complete bool,
) error {
	if params.ProjectID <= 0 || params.IdempotencyKey == "" ||
		params.Fingerprint == "" || strings.TrimSpace(params.ClaimToken) == "" || params.At.IsZero() {
		return errors.New("update close event delivery claim: invalid parameters")
	}
	return d.RetryTransient(ctx, func() error {
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin close event delivery update: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		state := "pending"
		deliveredAt := any(nil)
		if complete {
			state = "delivered"
			deliveredAt = params.At.UTC().Format(sqliteTimeFormat)
		}
		result, err := tx.ExecContext(ctx, `UPDATE close_event_deliveries
         SET state = ?, claim_token = NULL, claim_expires_at = NULL,
             delivered_at = ?, updated_at = ?
       WHERE project_id = ? AND idempotency_key = ? AND fingerprint = ?
         AND state = 'delivering' AND claim_token = ?`,
			state, deliveredAt, params.At.UTC().Format(sqliteTimeFormat),
			params.ProjectID, params.IdempotencyKey, params.Fingerprint, params.ClaimToken)
		if err != nil {
			return fmt.Errorf("update close event delivery: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read close event delivery update result: %w", err)
		}
		if affected == 0 {
			var fingerprint, currentState string
			err := tx.QueryRowContext(ctx, `SELECT fingerprint, state
          FROM close_event_deliveries WHERE project_id = ? AND idempotency_key = ?`,
				params.ProjectID, params.IdempotencyKey).Scan(&fingerprint, &currentState)
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read close event delivery after claim loss: %w", err)
			}
			if fingerprint != params.Fingerprint {
				return db.ErrCloseEventDeliveryFingerprintMismatch
			}
			if complete && currentState == "delivered" {
				return tx.Commit()
			}
			if !complete && currentState == "pending" {
				return tx.Commit()
			}
			return db.ErrCloseEventDeliveryClaimLost
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit close event delivery update: %w", err)
		}
		return nil
	})
}

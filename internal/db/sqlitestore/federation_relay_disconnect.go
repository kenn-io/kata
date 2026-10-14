package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RelaySelfRevocationEnrollmentID resolves a retained relay grant without
// requiring active grant, parent-token or project membership authority. The
// caller must still verify the grant's signed request before cleanup.
func (d *Store) RelaySelfRevocationEnrollmentID(ctx context.Context, token string, projectID int64) (int64, error) {
	if token == "" || projectID <= 0 {
		return 0, db.ErrNotFound
	}
	var enrollmentID int64
	err := d.QueryRowContext(ctx, `SELECT id FROM federation_enrollments
		WHERE token_hash=? AND project_id=? AND relay_protocol_version=1 AND parent_token_id IS NOT NULL`,
		db.FederationTokenHash(token), projectID,
	).Scan(&enrollmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, db.ErrNotFound
	}
	return enrollmentID, err
}

// RevokeOwnRelayEnrollment revokes a relay grant only for its authenticated parent account.
func (d *Store) RevokeOwnRelayEnrollment(ctx context.Context, token string, projectID int64, peer string) error {
	if token == "" || projectID <= 0 || !uid.Valid(peer) {
		return db.ErrNotFound
	}
	// UPDATE serializes with authenticated delivery transactions. Matching only
	// this secret, project and peer also makes revoked-grant retries safe. The
	// grant's delivery rows are removed with its authority.
	return d.relayTx(ctx, func(tx *sql.Tx) error {
		var bindingUID string
		err := tx.QueryRowContext(ctx, `UPDATE federation_enrollments SET revoked_at=COALESCE(revoked_at,strftime('%Y-%m-%dT%H:%M:%fZ','now')), updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE token_hash=? AND project_id=? AND spoke_instance_uid=? AND relay_protocol_version=1 AND parent_token_id IS NOT NULL RETURNING relay_binding_uid`, db.FederationTokenHash(token), projectID, peer).Scan(&bindingUID)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return err
		}
		return deleteRelayBindingStateTx(ctx, tx, bindingUID)
	})
}

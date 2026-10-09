package pgstore

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
		WHERE token_hash=$1 AND project_id=$2 AND relay_protocol_version=1 AND parent_token_id IS NOT NULL`,
		db.FederationTokenHash(token), projectID,
	).Scan(&enrollmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, db.ErrNotFound
	}
	return enrollmentID, mapSQLError(err, nil)
}

// RevokeOwnRelayEnrollment revokes a relay grant only for its authenticated parent account.
func (d *Store) RevokeOwnRelayEnrollment(ctx context.Context, token string, projectID int64, peer string) error {
	if token == "" || projectID <= 0 || !uid.Valid(peer) {
		return db.ErrNotFound
	}
	// UPDATE serializes with authenticated delivery transactions. Matching only
	// this secret, project and peer also makes revoked-grant retries safe.
	return d.relayTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE federation_enrollments SET revoked_at=COALESCE(revoked_at,to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')), updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') WHERE token_hash=$1 AND project_id=$2 AND spoke_instance_uid=$3 AND relay_protocol_version=1 AND parent_token_id IS NOT NULL`, db.FederationTokenHash(token), projectID, peer)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return db.ErrNotFound
		}
		return nil
	})
}

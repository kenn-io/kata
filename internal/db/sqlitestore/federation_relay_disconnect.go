package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RevokeOwnRelayEnrollment revokes a relay grant only for its authenticated parent account.
func (d *Store) RevokeOwnRelayEnrollment(ctx context.Context, token string, projectID int64, peer string) error {
	if token == "" || projectID <= 0 || !uid.Valid(peer) {
		return db.ErrNotFound
	}
	// UPDATE serializes with authenticated delivery transactions. Matching only
	// this secret, project and peer also makes revoked-grant retries safe.
	return d.relayTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE federation_enrollments SET revoked_at=COALESCE(revoked_at,strftime('%Y-%m-%dT%H:%M:%fZ','now')), updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE token_hash=? AND project_id=? AND spoke_instance_uid=? AND relay_protocol_version=1 AND parent_token_id IS NOT NULL`, db.FederationTokenHash(token), projectID, peer)
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

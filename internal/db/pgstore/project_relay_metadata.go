package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// deleteProjectRelayMetadataTx removes every meta row keyed by the project
// UID: relay reset state, root key transitions, pending creation intent and
// the attribution stream cursor. Purge, merge, orphan cleanup and UID adoption
// all retire the UID, so none of these rows may outlive it.
func deleteProjectRelayMetadataTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	resetKey := db.RelayResetMetadataPrefix + projectUID
	resetPrefix := resetKey + "."
	transitionPrefix := db.RootKeyTransitionMetadataPrefix + projectUID + "."
	pendingPrefix := db.PendingCreationMetadataPrefix + projectUID + "."
	supersededPrefix := db.RelayResetSupersededMetadataPrefix + projectUID + "."
	_, err := tx.ExecContext(ctx, `DELETE FROM meta
		WHERE key = $1
		   OR key = $2
		   OR key = $7
		   OR left(key, length($3)) = $3
		   OR left(key, length($4)) = $4
		   OR left(key, length($5)) = $5
		   OR left(key, length($6)) = $6`,
		resetKey, db.AttributionUIResetMetadataPrefix+projectUID, resetPrefix, transitionPrefix, pendingPrefix, supersededPrefix, db.ProjectStreamResetMetadataPrefix+projectUID)
	return mapSQLError(err, nil)
}

func retireProjectFederationUIDStateTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	for _, statement := range []struct{ table, query string }{
		{"federation_relay_outbox", `DELETE FROM federation_relay_outbox WHERE project_uid = $1`},
		{"federation_relay_inbox", `DELETE FROM federation_relay_inbox WHERE project_uid = $1`},
		{"federation_relay_cursors", `DELETE FROM federation_relay_cursors WHERE project_uid = $1`},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, projectUID); err != nil {
			return fmt.Errorf("retire project relay %s: %w", statement.table, mapSQLError(err, nil))
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM federation_root_keys WHERE project_uid = $1`, projectUID); err != nil {
		return fmt.Errorf("retire project root provenance: %w", mapSQLError(err, nil))
	}
	if err := deleteProjectRelayMetadataTx(ctx, tx, projectUID); err != nil {
		return fmt.Errorf("retire project relay metadata: %w", err)
	}
	return nil
}

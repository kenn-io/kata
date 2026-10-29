package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// deleteProjectRelayMetadata removes every meta row keyed by the project UID:
// relay reset state, root key transitions, pending creation intent and the
// attribution stream cursor. Purge, merge, orphan cleanup and UID adoption
// all retire the UID, so none of these rows may outlive it.
func deleteProjectRelayMetadata(ctx context.Context, execer contextExecer, projectUID string) error {
	resetKey := db.RelayResetMetadataPrefix + projectUID
	resetPrefix := resetKey + "."
	transitionPrefix := db.RootKeyTransitionMetadataPrefix + projectUID + "."
	pendingPrefix := db.PendingCreationMetadataPrefix + projectUID + "."
	supersededPrefix := db.RelayResetSupersededMetadataPrefix + projectUID + "."
	_, err := execer.ExecContext(ctx, `DELETE FROM meta
		WHERE key = ?
		   OR key = ?
		   OR key = ?
		   OR substr(key, 1, length(?)) = ?
		   OR substr(key, 1, length(?)) = ?
		   OR substr(key, 1, length(?)) = ?
		   OR substr(key, 1, length(?)) = ?`,
		resetKey, db.AttributionUIResetMetadataPrefix+projectUID, db.ProjectStreamResetMetadataPrefix+projectUID,
		resetPrefix, resetPrefix, transitionPrefix, transitionPrefix, pendingPrefix, pendingPrefix,
		supersededPrefix, supersededPrefix)
	return err
}

func retireProjectFederationUIDStateTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	for _, statement := range []struct{ table, query string }{
		{"federation_relay_outbox", `DELETE FROM federation_relay_outbox WHERE project_uid = ?`},
		{"federation_relay_inbox", `DELETE FROM federation_relay_inbox WHERE project_uid = ?`},
		{"federation_relay_cursors", `DELETE FROM federation_relay_cursors WHERE project_uid = ?`},
	} {
		if _, err := tx.ExecContext(ctx, statement.query, projectUID); err != nil {
			return fmt.Errorf("retire project relay %s: %w", statement.table, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM federation_root_keys WHERE project_uid = ?`, projectUID); err != nil {
		return fmt.Errorf("retire project root provenance: %w", err)
	}
	if err := deleteProjectRelayMetadata(ctx, tx, projectUID); err != nil {
		return fmt.Errorf("retire project relay metadata: %w", err)
	}
	return nil
}

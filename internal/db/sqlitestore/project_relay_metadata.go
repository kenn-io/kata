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

func deleteProjectRelayMetadata(ctx context.Context, execer contextExecer, projectUID string) error {
	resetKey := db.RelayResetMetadataPrefix + projectUID
	resetPrefix := resetKey + "."
	transitionPrefix := db.RootKeyTransitionMetadataPrefix + projectUID + "."
	_, err := execer.ExecContext(ctx, `DELETE FROM meta
		WHERE key = ?
		   OR substr(key, 1, length(?)) = ?
		   OR substr(key, 1, length(?)) = ?`,
		resetKey, resetPrefix, resetPrefix, transitionPrefix, transitionPrefix)
	return err
}

func retireProjectFederationUIDStateTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	for _, table := range []string{
		"federation_relay_outbox",
		"federation_relay_inbox",
		"federation_relay_cursors",
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE project_uid = ?`, projectUID); err != nil {
			return fmt.Errorf("retire project relay %s: %w", table, err)
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

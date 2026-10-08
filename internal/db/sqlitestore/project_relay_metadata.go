package sqlitestore

import (
	"context"
	"database/sql"

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

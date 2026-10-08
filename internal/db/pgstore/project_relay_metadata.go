package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

func deleteProjectRelayMetadataTx(ctx context.Context, tx *sql.Tx, projectUID string) error {
	resetKey := db.RelayResetMetadataPrefix + projectUID
	resetPrefix := resetKey + "."
	transitionPrefix := db.RootKeyTransitionMetadataPrefix + projectUID + "."
	_, err := tx.ExecContext(ctx, `DELETE FROM meta
		WHERE key = $1
		   OR left(key, length($2)) = $2
		   OR left(key, length($3)) = $3`,
		resetKey, resetPrefix, transitionPrefix)
	return mapSQLError(err, nil)
}

package sqlitestore

import (
	"context"
	"strconv"

	"go.kenn.io/kata/internal/db"
)

// Reserve in the receipt transaction so projection readers cannot observe a
// verified creator with an old validator. Source event bytes remain unchanged.
func reserveAttributionUIResetTx(ctx context.Context, tx db.Transaction, projectUID string) error {
	cursor, err := reserveEventSequence(ctx, tx, true)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, db.AttributionUIResetMetadataPrefix+projectUID, strconv.FormatInt(cursor.Int64, 10))
	return err
}

func (d *Store) attributionUIResetAfter(ctx context.Context, afterID, projectID int64) (int64, error) {
	args := []any{afterID}
	query := `SELECT COALESCE(MAX(CAST(m.value AS BIGINT)),0) FROM meta m JOIN projects p ON m.key='attribution_ui_reset.'||p.uid WHERE CAST(m.value AS BIGINT)>?`
	if projectID != 0 {
		query += ` AND p.id=?`
		args = append(args, projectID)
	}
	query += " AND " + authorizedProjectPredicate(ctx, "p.uid", &args)
	var cursor int64
	err := d.QueryRowContext(ctx, query, args...).Scan(&cursor)
	return cursor, err
}

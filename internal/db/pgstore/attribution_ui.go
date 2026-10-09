package pgstore

import (
	"context"
	"strconv"

	"go.kenn.io/kata/internal/db"
)

// The configured store search_path resolves its events identity sequence.
// Sequence gaps on a rolled-back transaction grant no visible reset authority.
func reserveAttributionUIResetTx(ctx context.Context, tx db.Transaction, projectUID string) error {
	// Share commit ordering with events and purges, so a later event cannot
	// become visible and move a browser cursor past this uncommitted reset.
	if err := lockEventSequenceTx(ctx, tx); err != nil {
		return err
	}
	var cursor int64
	if err := tx.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('events','id')::regclass)`).Scan(&cursor); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, db.AttributionUIResetMetadataPrefix+projectUID, strconv.FormatInt(cursor, 10))
	return err
}

func (d *Store) attributionUIResetAfter(ctx context.Context, afterID, projectID int64) (int64, error) {
	args := []any{afterID}
	// PostgreSQL may evaluate a WHERE cast before discarding nonmatching
	// join rows. Guard the cast itself; malformed actual reset values still fail.
	resetValue := `CASE WHEN m.key='attribution_ui_reset.'||p.uid THEN CAST(m.value AS BIGINT) END`
	query := `SELECT COALESCE(MAX(` + resetValue + `),0) FROM meta m JOIN projects p ON m.key='attribution_ui_reset.'||p.uid WHERE ` + resetValue + `>$1`
	if projectID != 0 {
		query += ` AND p.id=$2`
		args = append(args, projectID)
	}
	query += " AND " + authorizedProjectPredicate(ctx, "p.uid", &args)
	var cursor int64
	err := d.QueryRowContext(ctx, query, args...).Scan(&cursor)
	return cursor, err
}

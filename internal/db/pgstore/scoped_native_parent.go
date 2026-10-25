package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// deleteScopedLinkByID keeps the bare row deletion and any existing parent
// predicate update atomic. Its caller retries this complete transaction.
func (s *Store) deleteScopedLinkByID(ctx context.Context, linkID int64) error {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var childID, parentID int64
	var kind string
	err = tx.QueryRowContext(ctx, `DELETE FROM links WHERE id=$1 RETURNING from_issue_id,to_issue_id,type`, linkID).Scan(&childID, &parentID, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return mapSQLError(err, nil)
	}
	if kind == "parent" {
		if err := s.recordScopedNativeParentMutationTx(ctx, tx, childID, parentID); err != nil {
			return err
		}
	}
	return mapSQLError(tx.Commit(), nil)
}

// recordScopedNativeParentMutationTx composes existing issuer freshness and
// root capture work independently. The actual native endpoints supply both
// affected projects; absence of an issuer row never suppresses dirty work.
func (s *Store) recordScopedNativeParentMutationTx(ctx context.Context, tx *sql.Tx, childID, parentID int64) error {
	if err := db.RecordScopedNativeParentPredicateTx(ctx, tx, s.instanceUID, childID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.id IN ($1,$2) ORDER BY p.uid`, childID, parentID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var projects []string
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return s.markScopedCanonicalDirtyTx(ctx, tx, projects...)
}

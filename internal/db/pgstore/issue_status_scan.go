package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

var _ db.IssueStatusScanStore = (*Store)(nil)

// UpdateIssueStatusScan stores daemon-owned scan progress under the binding claim.
func (s *Store) UpdateIssueStatusScan(ctx context.Context, guard db.IssueSyncImportGuard, state db.IssueStatusScanState) (db.IssueSyncBinding, error) {
	var binding db.IssueSyncBinding
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		var err error
		binding, err = updateIssueStatusScanTx(ctx, tx, guard, state)
		return err
	})
	return binding, err
}
func updateIssueStatusScanTx(ctx context.Context, tx *sql.Tx, guard db.IssueSyncImportGuard, state db.IssueStatusScanState) (db.IssueSyncBinding, error) {
	binding, err := statusReadBindingTx(ctx, tx, guard)
	if err != nil {
		return db.IssueSyncBinding{}, err
	}
	config, err := db.SetIssueStatusScanConfig(binding.Config, state)
	if err != nil {
		return db.IssueSyncBinding{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE issue_sync_bindings SET config_json=$1 WHERE id=$2`, string(config), binding.ID)
	if err != nil {
		return db.IssueSyncBinding{}, err
	}
	binding.Config = config
	return binding, nil
}

// CountPendingIssueStatuses counts mapped issues with undelivered local events.
func (s *Store) CountPendingIssueStatuses(ctx context.Context, bindingID int64) (int, error) {
	var count int
	err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_mappings m JOIN issue_sync_bindings b ON b.project_id=m.project_id AND b.source_key=m.source JOIN issues i ON i.id=m.issue_id
 WHERE b.id=$1 AND m.object_type='issue' AND m.pending_event_uid IS NOT NULL AND i.deleted_at IS NULL`, bindingID).Scan(&count)
	return count, err
}

var _ db.IssueStatusSummaryReader = (*Store)(nil)

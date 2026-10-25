package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// scopedCapacityTx measures actual retained rows after candidate writes, inside
// the same transaction. Admission never compacts or evicts owed representations.
func (s *Store) scopedCapacityTx(ctx context.Context, tx *sql.Tx, viewUID string) error {
	var work db.ScopedWorkExport
	if err := tx.QueryRowContext(ctx, `SELECT view_uid,dirty_revision,captured_revision,capture_uid,capture_bytes,capture_digest,state FROM federation_scoped_work WHERE view_uid=$1`, viewUID).Scan(&work.ViewUID, &work.DirtyRevision, &work.CapturedRevision, &work.CaptureUID, &work.CaptureBytes, &work.CaptureDigest, &work.State); err != nil {
		return err
	}
	// Reuse the native capture/recovery accountant and shared byte widths.
	if err := scopedCaptureBudgetTx(ctx, tx, viewUID, work); err != nil {
		return err
	}
	// Match the accepted restore accountant: per-binding bytes cover queued
	// deliveries and staged source/control custody, not active materialization.
	bindings := map[string]int64{}
	operations := map[string]string{}
	operationBytes := map[string]int64{}
	for record, err := range db.ExportScopedStateSQL(ctx, tx.QueryContext) {
		if err != nil {
			return err
		}
		var binding string
		switch r := record.(type) {
		case *db.ScopedGenerationExport:
			if r.AssembledBytes > db.MaxScopedCheckpointBytes {
				return errors.New("scoped assembled checkpoint exceeds capacity")
			}
		case *db.ScopedDeliveryRecord:
			binding = r.BindingUID
		case *db.ScopedControlCustodyExport:
			var principal db.CustodyPrincipal
			if err := db.DecodeScopedRecord(r.PrincipalBytes, &principal); err != nil {
				return err
			}
			binding = principal.BindingUID
		case *db.ScopedSourceOperationExport:
			var principal db.CustodyPrincipal
			if err := db.DecodeScopedRecord(r.PrincipalBytes, &principal); err != nil {
				return err
			}
			operations[r.OperationUID] = principal.BindingUID
			operationBytes[r.OperationUID] += db.ScopedRetainedBytes(record)
		case *db.ScopedPrivateSourceExport:
			if r.OperationUID != nil {
				operationBytes[*r.OperationUID] += db.ScopedRetainedBytes(record)
			}
		}
		if binding != "" {
			bindings[binding] += db.ScopedRetainedBytes(record)
		}
	}
	for operation, size := range operationBytes {
		bindings[operations[operation]] += size
	}
	for _, size := range bindings {
		if size > db.MaxScopedBindingBytes {
			return errors.New("scoped binding capacity exceeded; retained obligations stay pinned")
		}
	}
	return nil
}

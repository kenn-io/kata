package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

var _ db.TrackerStatusStore = (*Store)(nil)

// AdmitTrackerStatusWrite atomically validates and admits a named status write.
func (s *Store) AdmitTrackerStatusWrite(ctx context.Context, p db.TrackerStatusAdmission) (db.TrackerExecutionFence, error) {
	var fence db.TrackerExecutionFence
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		fence = db.TrackerExecutionFence{}
		binding, err := statusReadBindingTx(ctx, tx, p.Guard)
		if err != nil {
			return err
		}
		mapping, err := loadIssueStatusMappingTx(ctx, tx, binding, p.MappingID)
		if err != nil {
			return err
		}
		fence, err = s.trackerFenceSQL().AdmitStatusInTransaction(ctx, tx, binding, mapping, p)
		return err
	})
	return fence, err
}

// PrepareTrackerStatusRetry prepares a witnessed root retry without dispatch.
func (s *Store) PrepareTrackerStatusRetry(ctx context.Context, p db.TrackerStatusAdmission, retry db.TrackerRemoteRetryPreparation) (db.TrackerExecutionFence, error) {
	var fence db.TrackerExecutionFence
	err := s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		fence = db.TrackerExecutionFence{}
		binding, err := statusReadBindingTx(ctx, tx, p.Guard)
		if err != nil {
			return err
		}
		mapping, err := loadIssueStatusMappingTx(ctx, tx, binding, p.MappingID)
		if err != nil {
			return err
		}
		fence, err = s.trackerFenceSQL().PrepareStatusRetryInTransaction(ctx, tx, binding, mapping, p, retry)
		return err
	})
	return fence, err
}

// AcknowledgeRetainedTrackerStatus applies a retained status receipt under the import guard.
func (s *Store) AcknowledgeRetainedTrackerStatus(ctx context.Context, guard db.IssueSyncImportGuard) error {
	return s.withSerializableTx(ctx, func(tx *sql.Tx) error {
		binding, err := statusReadBindingTx(ctx, tx, guard)
		if err != nil {
			return err
		}
		return s.trackerFenceSQL().AcknowledgeRetainedStatusInTransaction(ctx, tx, binding)
	})
}

package sqlitestore

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

var _ db.TrackerStatusStore = (*Store)(nil)

func (s *Store) AdmitTrackerStatusWrite(ctx context.Context, p db.TrackerStatusAdmission) (db.TrackerExecutionFence, error) {
	return retryWrite1(ctx, s, func() (db.TrackerExecutionFence, error) {
		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, err := statusReadBindingTx(ctx, tx, p.Guard)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		mapping, err := loadIssueStatusMappingTx(ctx, tx, binding, p.MappingID)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		fence, err := s.trackerFenceSQL().AdmitStatusInTransaction(ctx, tx, binding, mapping, p)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.TrackerExecutionFence{}, err
		}
		return fence, nil
	})
}

// PrepareTrackerStatusRetry prepares a witnessed root retry without dispatch.
func (s *Store) PrepareTrackerStatusRetry(ctx context.Context, p db.TrackerStatusAdmission, retry db.TrackerRemoteRetryPreparation) (db.TrackerExecutionFence, error) {
	return retryWrite1(ctx, s, func() (db.TrackerExecutionFence, error) {
		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		defer func() { _ = tx.Rollback() }()
		binding, err := statusReadBindingTx(ctx, tx, p.Guard)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		mapping, err := loadIssueStatusMappingTx(ctx, tx, binding, p.MappingID)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		fence, err := s.trackerFenceSQL().PrepareStatusRetryInTransaction(ctx, tx, binding, mapping, p, retry)
		if err != nil {
			return db.TrackerExecutionFence{}, err
		}
		if err := tx.Commit(); err != nil {
			return db.TrackerExecutionFence{}, err
		}
		return fence, nil
	})
}

func (s *Store) AcknowledgeRetainedTrackerStatus(ctx context.Context, guard db.IssueSyncImportGuard) error {
	return s.RetryTransient(ctx, func() error {
		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		binding, err := statusReadBindingTx(ctx, tx, guard)
		if err != nil {
			return err
		}
		if err := s.trackerFenceSQL().AcknowledgeRetainedStatusInTransaction(ctx, tx, binding); err != nil {
			return err
		}
		return tx.Commit()
	})
}

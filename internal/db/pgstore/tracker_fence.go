package pgstore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

func (s *Store) trackerFenceSQL() db.TrackerFenceSQL {
	return db.TrackerFenceSQL{Postgres: true, InstanceUID: s.InstanceUID(), Transact: func(ctx context.Context, operation func(*sql.Tx) error) error {
		return s.withSerializableTx(ctx, operation)
	}}
}

func (s *Store) LoadTrackerExecutionFence(ctx context.Context, rootUID, projectUID, role string) (db.TrackerExecutionFence, bool, error) {
	return s.trackerFenceSQL().Load(ctx, rootUID, projectUID, role)
}

func (s *Store) StoreTrackerExecutionFence(ctx context.Context, previous *db.TrackerExecutionFence, next db.TrackerExecutionFence, processEpoch string) error {
	return s.trackerFenceSQL().Store(ctx, previous, next, processEpoch)
}

func (s *Store) ReconcileTrackerRegistry(ctx context.Context, epoch string, policies []tracker.RegistryPolicy) ([]tracker.RegistryRecord, error) {
	return s.trackerFenceSQL().ReconcileRegistry(ctx, epoch, policies)
}

func (s *Store) LoadTrackerActivation(ctx context.Context, projectUID string) (db.TrackerActivation, bool, error) {
	return s.trackerFenceSQL().LoadActivation(ctx, projectUID)
}
func (s *Store) ActivateTrackerBinding(ctx context.Context, bindingID int64, updatedAt time.Time, epoch string) (db.TrackerActivation, error) {
	return s.trackerFenceSQL().Activate(ctx, bindingID, updatedAt, epoch)
}

func (s *Store) BeginTrackerControl(ctx context.Context, p tracker.ControlReceipt) (tracker.ControlReceipt, bool, error) {
	return s.trackerFenceSQL().BeginControl(ctx, p)
}
func (s *Store) CompleteTrackerControl(ctx context.Context, previous, next tracker.ControlReceipt) error {
	return s.trackerFenceSQL().CompleteControl(ctx, previous, next)
}
func (s *Store) ReadTrackerControl(ctx context.Context, project, operation string) (tracker.ControlReceipt, error) {
	return s.trackerFenceSQL().ReadControl(ctx, project, operation)
}

func (s *Store) TrackerRestoreHistoryBlocked(ctx context.Context, project string) (bool, error) {
	return s.trackerFenceSQL().RestoreHistoryBlocked(ctx, project)
}

func (s *Store) LoadTrackerOwnerCheckpoint(ctx context.Context, root, project, owner string) (db.TrackerOwnerCheckpoint, bool, error) {
	return s.trackerFenceSQL().LoadOwnerCheckpoint(ctx, root, project, owner)
}

func (s *Store) SettleTrackerOwnerNonSend(ctx context.Context, owner db.TrackerExecutionFence) error {
	return s.trackerFenceSQL().SettleOwnerNonSend(ctx, owner)
}
func (s *Store) AcceptTrackerOwnerResult(ctx context.Context, owner db.TrackerExecutionFence) error {
	return s.trackerFenceSQL().AcceptOwnerResult(ctx, owner)
}
func (s *Store) RetireTrackerExecutorPreparation(ctx context.Context, p db.TrackerExecutionFence, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().RetireExecutorPreparation(ctx, p, epoch)
}

func (s *Store) PrepareTrackerExecutor(ctx context.Context, root db.TrackerExecutionFence, provider, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().PrepareExecutor(ctx, root, provider, epoch)
}

// PrepareTrackerExecutorRetry prepares a witnessed owner retry without dispatch.
func (s *Store) PrepareTrackerExecutorRetry(ctx context.Context, p db.TrackerRemoteRetryPreparation, provider, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().PrepareExecutorRetry(ctx, p, provider, epoch)
}
func (s *Store) AdmitTrackerExecutor(ctx context.Context, prepared db.TrackerExecutionFence, provider, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().AdmitExecutor(ctx, prepared, provider, epoch)
}
func (s *Store) AcknowledgeTrackerExecutor(ctx context.Context, checkpoint db.TrackerOwnerCheckpoint) error {
	return s.trackerFenceSQL().AcknowledgeExecutor(ctx, checkpoint)
}

func (s *Store) LoadTrackerExecutionHistory(ctx context.Context, root, project, role, write string) (db.TrackerExecutionFence, bool, error) {
	return s.trackerFenceSQL().LoadHistory(ctx, root, project, role, write)
}

// LoadTrackerExecutionAttempt reads a retained non-send attempt by exact sequence.
func (s *Store) LoadTrackerExecutionAttempt(ctx context.Context, root, project, role, write string, sequence uint64) (db.TrackerExecutionFence, bool, error) {
	return s.trackerFenceSQL().LoadAttempt(ctx, root, project, role, write, sequence)
}

func (s *Store) AcknowledgeTrackerRecovery(ctx context.Context, project string) error {
	return s.trackerFenceSQL().AcknowledgeRecovery(ctx, project)
}

func (s *Store) RetireTrackerCoordinatorPreparation(ctx context.Context, p db.TrackerExecutionFence, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().RetireCoordinatorPreparation(ctx, p, epoch)
}

func (s *Store) ListTrackerOwnerCheckpoints(ctx context.Context, root, project, after string, limit int) ([]db.TrackerOwnerCheckpoint, error) {
	return s.trackerFenceSQL().ListOwnerCheckpoints(ctx, root, project, after, limit)
}

func (s *Store) ListTrackerRecoveryHistory(ctx context.Context, root, project, after string, limit int) ([]db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().ListRecoveryHistory(ctx, root, project, after, limit)
}

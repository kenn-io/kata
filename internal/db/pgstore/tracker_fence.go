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

// LoadTrackerExecutionFence reads the current execution head for a lane.
func (s *Store) LoadTrackerExecutionFence(ctx context.Context, rootUID, projectUID, role string) (db.TrackerExecutionFence, bool, error) {
	return s.trackerFenceSQL().Load(ctx, rootUID, projectUID, role)
}

// StoreTrackerExecutionFence atomically advances a matching execution fence.
func (s *Store) StoreTrackerExecutionFence(ctx context.Context, previous *db.TrackerExecutionFence, next db.TrackerExecutionFence, processEpoch string) error {
	return s.trackerFenceSQL().Store(ctx, previous, next, processEpoch)
}

// ReconcileTrackerRegistry reconciles connection generations for a process epoch.
func (s *Store) ReconcileTrackerRegistry(ctx context.Context, epoch string, policies []tracker.RegistryPolicy) ([]tracker.RegistryRecord, error) {
	return s.trackerFenceSQL().ReconcileRegistry(ctx, epoch, policies)
}

// LoadTrackerActivation reads the local activation state for a project.
func (s *Store) LoadTrackerActivation(ctx context.Context, projectUID string) (db.TrackerActivation, bool, error) {
	return s.trackerFenceSQL().LoadActivation(ctx, projectUID)
}

// ActivateTrackerBinding activates the exact binding revision for a process epoch.
func (s *Store) ActivateTrackerBinding(ctx context.Context, bindingID int64, updatedAt time.Time, epoch string) (db.TrackerActivation, error) {
	return s.trackerFenceSQL().Activate(ctx, bindingID, updatedAt, epoch)
}

// BeginTrackerControl retains a control receipt before its operation begins.
func (s *Store) BeginTrackerControl(ctx context.Context, p tracker.ControlReceipt) (tracker.ControlReceipt, bool, error) {
	return s.trackerFenceSQL().BeginControl(ctx, p)
}

// CompleteTrackerControl atomically replaces a matching control receipt.
func (s *Store) CompleteTrackerControl(ctx context.Context, previous, next tracker.ControlReceipt) error {
	return s.trackerFenceSQL().CompleteControl(ctx, previous, next)
}

// ReadTrackerControl reads a project-scoped control receipt.
func (s *Store) ReadTrackerControl(ctx context.Context, project, operation string) (tracker.ControlReceipt, error) {
	return s.trackerFenceSQL().ReadControl(ctx, project, operation)
}

// TrackerRestoreHistoryBlocked reports whether restored history blocks provider writes.
func (s *Store) TrackerRestoreHistoryBlocked(ctx context.Context, project string) (bool, error) {
	return s.trackerFenceSQL().RestoreHistoryBlocked(ctx, project)
}

// LoadTrackerOwnerCheckpoint reads the retained acknowledgement for an owner.
func (s *Store) LoadTrackerOwnerCheckpoint(ctx context.Context, root, project, owner string) (db.TrackerOwnerCheckpoint, bool, error) {
	return s.trackerFenceSQL().LoadOwnerCheckpoint(ctx, root, project, owner)
}

// SettleTrackerOwnerNonSend records definitive evidence that an owner did not send.
func (s *Store) SettleTrackerOwnerNonSend(ctx context.Context, owner db.TrackerExecutionFence) error {
	return s.trackerFenceSQL().SettleOwnerNonSend(ctx, owner)
}

// AcceptTrackerOwnerResult accepts an acknowledged owner result into retained history.
func (s *Store) AcceptTrackerOwnerResult(ctx context.Context, owner db.TrackerExecutionFence) error {
	return s.trackerFenceSQL().AcceptOwnerResult(ctx, owner)
}

// RetireTrackerExecutorPreparation retires an executor preparation without dispatch.
func (s *Store) RetireTrackerExecutorPreparation(ctx context.Context, p db.TrackerExecutionFence, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().RetireExecutorPreparation(ctx, p, epoch)
}

// PrepareTrackerExecutor prepares an executor attempt for an authorized root.
func (s *Store) PrepareTrackerExecutor(ctx context.Context, root db.TrackerExecutionFence, provider, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().PrepareExecutor(ctx, root, provider, epoch)
}

// PrepareTrackerExecutorRetry prepares a witnessed owner retry without dispatch.
func (s *Store) PrepareTrackerExecutorRetry(ctx context.Context, p db.TrackerRemoteRetryPreparation, provider, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().PrepareExecutorRetry(ctx, p, provider, epoch)
}

// AdmitTrackerExecutor admits a prepared executor attempt for dispatch.
func (s *Store) AdmitTrackerExecutor(ctx context.Context, prepared db.TrackerExecutionFence, provider, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().AdmitExecutor(ctx, prepared, provider, epoch)
}

// AcknowledgeTrackerExecutor records the owner checkpoint for an executor result.
func (s *Store) AcknowledgeTrackerExecutor(ctx context.Context, checkpoint db.TrackerOwnerCheckpoint) error {
	return s.trackerFenceSQL().AcknowledgeExecutor(ctx, checkpoint)
}

// LoadTrackerExecutionHistory reads retained terminal evidence for a write.
func (s *Store) LoadTrackerExecutionHistory(ctx context.Context, root, project, role, write string) (db.TrackerExecutionFence, bool, error) {
	return s.trackerFenceSQL().LoadHistory(ctx, root, project, role, write)
}

// LoadTrackerExecutionAttempt reads a retained non-send attempt by exact sequence.
func (s *Store) LoadTrackerExecutionAttempt(ctx context.Context, root, project, role, write string, sequence uint64) (db.TrackerExecutionFence, bool, error) {
	return s.trackerFenceSQL().LoadAttempt(ctx, root, project, role, write, sequence)
}

// AcknowledgeTrackerRecovery clears a project restore barrier after reconciliation.
func (s *Store) AcknowledgeTrackerRecovery(ctx context.Context, project string) error {
	return s.trackerFenceSQL().AcknowledgeRecovery(ctx, project)
}

// RetireTrackerCoordinatorPreparation retires an undispatched coordinator preparation.
func (s *Store) RetireTrackerCoordinatorPreparation(ctx context.Context, p db.TrackerExecutionFence, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().RetireCoordinatorPreparation(ctx, p, epoch)
}

// ListTrackerOwnerCheckpoints lists retained owner acknowledgements after a cursor.
func (s *Store) ListTrackerOwnerCheckpoints(ctx context.Context, root, project, after string, limit int) ([]db.TrackerOwnerCheckpoint, error) {
	return s.trackerFenceSQL().ListOwnerCheckpoints(ctx, root, project, after, limit)
}

// ListTrackerRecoveryHistory lists retained executor recovery evidence after a cursor.
func (s *Store) ListTrackerRecoveryHistory(ctx context.Context, root, project, after string, limit int) ([]db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().ListRecoveryHistory(ctx, root, project, after, limit)
}

package sqlitestore

import (
	"context"
	"database/sql"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

func (d *Store) trackerFenceSQL() db.TrackerFenceSQL {
	return db.TrackerFenceSQL{InstanceUID: d.InstanceUID(), Transact: func(ctx context.Context, run func(*sql.Tx) error) error {
		return d.RetryTransient(ctx, func() error {
			tx, err := d.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := run(tx); err != nil {
				return err
			}
			return tx.Commit()
		})
	}}
}

// LoadTrackerExecutionFence reads the current execution head for a lane.
func (d *Store) LoadTrackerExecutionFence(ctx context.Context, rootUID, projectUID, role string) (db.TrackerExecutionFence, bool, error) {
	return d.trackerFenceSQL().Load(ctx, rootUID, projectUID, role)
}

// StoreTrackerExecutionFence atomically advances a matching execution fence.
func (d *Store) StoreTrackerExecutionFence(ctx context.Context, previous *db.TrackerExecutionFence, next db.TrackerExecutionFence, processEpoch string) error {
	return d.trackerFenceSQL().Store(ctx, previous, next, processEpoch)
}

// ReconcileTrackerRegistry reconciles connection generations for a process epoch.
func (d *Store) ReconcileTrackerRegistry(ctx context.Context, epoch string, policies []tracker.RegistryPolicy) ([]tracker.RegistryRecord, error) {
	return d.trackerFenceSQL().ReconcileRegistry(ctx, epoch, policies)
}

// LoadTrackerActivation reads the local activation state for a project.
func (d *Store) LoadTrackerActivation(ctx context.Context, projectUID string) (db.TrackerActivation, bool, error) {
	return d.trackerFenceSQL().LoadActivation(ctx, projectUID)
}

// ActivateTrackerBinding activates the exact binding revision for a process epoch.
func (d *Store) ActivateTrackerBinding(ctx context.Context, bindingID int64, updatedAt time.Time, epoch string) (db.TrackerActivation, error) {
	return d.trackerFenceSQL().Activate(ctx, bindingID, updatedAt, epoch)
}

// BeginTrackerControl retains a control receipt before its operation begins.
func (d *Store) BeginTrackerControl(ctx context.Context, p tracker.ControlReceipt) (tracker.ControlReceipt, bool, error) {
	return d.trackerFenceSQL().BeginControl(ctx, p)
}

// CompleteTrackerControl atomically replaces a matching control receipt.
func (d *Store) CompleteTrackerControl(ctx context.Context, previous, next tracker.ControlReceipt) error {
	return d.trackerFenceSQL().CompleteControl(ctx, previous, next)
}

// ReadTrackerControl reads a project-scoped control receipt.
func (d *Store) ReadTrackerControl(ctx context.Context, project, operation string) (tracker.ControlReceipt, error) {
	return d.trackerFenceSQL().ReadControl(ctx, project, operation)
}

// TrackerRestoreHistoryBlocked reports whether restored history blocks provider writes.
func (d *Store) TrackerRestoreHistoryBlocked(ctx context.Context, project string) (bool, error) {
	return d.trackerFenceSQL().RestoreHistoryBlocked(ctx, project)
}

// LoadTrackerOwnerCheckpoint reads the retained acknowledgement for an owner.
func (d *Store) LoadTrackerOwnerCheckpoint(ctx context.Context, root, project, owner string) (db.TrackerOwnerCheckpoint, bool, error) {
	return d.trackerFenceSQL().LoadOwnerCheckpoint(ctx, root, project, owner)
}

// SettleTrackerOwnerNonSend records definitive evidence that an owner did not send.
func (d *Store) SettleTrackerOwnerNonSend(ctx context.Context, owner db.TrackerExecutionFence) error {
	return d.trackerFenceSQL().SettleOwnerNonSend(ctx, owner)
}

// AcceptTrackerOwnerResult accepts an acknowledged owner result into retained history.
func (d *Store) AcceptTrackerOwnerResult(ctx context.Context, owner db.TrackerExecutionFence) error {
	return d.trackerFenceSQL().AcceptOwnerResult(ctx, owner)
}

// RetireTrackerExecutorPreparation retires an executor preparation without dispatch.
func (d *Store) RetireTrackerExecutorPreparation(ctx context.Context, p db.TrackerExecutionFence, epoch string) (db.TrackerExecutionFence, error) {
	return d.trackerFenceSQL().RetireExecutorPreparation(ctx, p, epoch)
}

// PrepareTrackerExecutor prepares an executor attempt for an authorized root.
func (d *Store) PrepareTrackerExecutor(ctx context.Context, root db.TrackerExecutionFence, provider, epoch string) (db.TrackerExecutionFence, error) {
	return d.trackerFenceSQL().PrepareExecutor(ctx, root, provider, epoch)
}

// PrepareTrackerExecutorRetry prepares a witnessed owner retry without dispatch.
func (d *Store) PrepareTrackerExecutorRetry(ctx context.Context, p db.TrackerRemoteRetryPreparation, provider, epoch string) (db.TrackerExecutionFence, error) {
	return d.trackerFenceSQL().PrepareExecutorRetry(ctx, p, provider, epoch)
}

// AdmitTrackerExecutor admits a prepared executor attempt for dispatch.
func (d *Store) AdmitTrackerExecutor(ctx context.Context, prepared db.TrackerExecutionFence, provider, epoch string) (db.TrackerExecutionFence, error) {
	return d.trackerFenceSQL().AdmitExecutor(ctx, prepared, provider, epoch)
}

// AcknowledgeTrackerExecutor records the owner checkpoint for an executor result.
func (d *Store) AcknowledgeTrackerExecutor(ctx context.Context, checkpoint db.TrackerOwnerCheckpoint) error {
	return d.trackerFenceSQL().AcknowledgeExecutor(ctx, checkpoint)
}

// LoadTrackerExecutionHistory reads retained terminal evidence for a write.
func (d *Store) LoadTrackerExecutionHistory(ctx context.Context, root, project, role, write string) (db.TrackerExecutionFence, bool, error) {
	return d.trackerFenceSQL().LoadHistory(ctx, root, project, role, write)
}

// LoadTrackerExecutionAttempt reads a retained non-send attempt by exact sequence.
func (d *Store) LoadTrackerExecutionAttempt(ctx context.Context, root, project, role, write string, sequence uint64) (db.TrackerExecutionFence, bool, error) {
	return d.trackerFenceSQL().LoadAttempt(ctx, root, project, role, write, sequence)
}

// AcknowledgeTrackerRecovery clears a project restore barrier after reconciliation.
func (d *Store) AcknowledgeTrackerRecovery(ctx context.Context, project string) error {
	return d.trackerFenceSQL().AcknowledgeRecovery(ctx, project)
}

// RetireTrackerCoordinatorPreparation retires an undispatched coordinator preparation.
func (d *Store) RetireTrackerCoordinatorPreparation(ctx context.Context, p db.TrackerExecutionFence, epoch string) (db.TrackerExecutionFence, error) {
	return d.trackerFenceSQL().RetireCoordinatorPreparation(ctx, p, epoch)
}

// ListTrackerOwnerCheckpoints lists retained owner acknowledgements after a cursor.
func (d *Store) ListTrackerOwnerCheckpoints(ctx context.Context, root, project, after string, limit int) ([]db.TrackerOwnerCheckpoint, error) {
	return d.trackerFenceSQL().ListOwnerCheckpoints(ctx, root, project, after, limit)
}

// ListTrackerRecoveryHistory lists retained executor recovery evidence after a cursor.
func (d *Store) ListTrackerRecoveryHistory(ctx context.Context, root, project, after string, limit int) ([]db.TrackerExecutionFence, error) {
	return d.trackerFenceSQL().ListRecoveryHistory(ctx, root, project, after, limit)
}

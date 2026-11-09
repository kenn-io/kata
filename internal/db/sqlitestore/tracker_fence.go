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

func (d *Store) LoadTrackerExecutionFence(ctx context.Context, rootUID, projectUID, role string) (db.TrackerExecutionFence, bool, error) {
	return d.trackerFenceSQL().Load(ctx, rootUID, projectUID, role)
}

func (d *Store) StoreTrackerExecutionFence(ctx context.Context, previous *db.TrackerExecutionFence, next db.TrackerExecutionFence, processEpoch string) error {
	return d.trackerFenceSQL().Store(ctx, previous, next, processEpoch)
}

func (d *Store) ReconcileTrackerRegistry(ctx context.Context, epoch string, policies []tracker.RegistryPolicy) ([]tracker.RegistryRecord, error) {
	return d.trackerFenceSQL().ReconcileRegistry(ctx, epoch, policies)
}

func (d *Store) LoadTrackerActivation(ctx context.Context, projectUID string) (db.TrackerActivation, bool, error) {
	return d.trackerFenceSQL().LoadActivation(ctx, projectUID)
}
func (d *Store) ActivateTrackerBinding(ctx context.Context, bindingID int64, updatedAt time.Time, epoch string) (db.TrackerActivation, error) {
	return d.trackerFenceSQL().Activate(ctx, bindingID, updatedAt, epoch)
}

func (d *Store) BeginTrackerControl(ctx context.Context, p tracker.ControlReceipt) (tracker.ControlReceipt, bool, error) {
	return d.trackerFenceSQL().BeginControl(ctx, p)
}
func (d *Store) CompleteTrackerControl(ctx context.Context, previous, next tracker.ControlReceipt) error {
	return d.trackerFenceSQL().CompleteControl(ctx, previous, next)
}
func (d *Store) ReadTrackerControl(ctx context.Context, project, operation string) (tracker.ControlReceipt, error) {
	return d.trackerFenceSQL().ReadControl(ctx, project, operation)
}

func (d *Store) TrackerRestoreHistoryBlocked(ctx context.Context, project string) (bool, error) {
	return d.trackerFenceSQL().RestoreHistoryBlocked(ctx, project)
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
func (s *Store) AdmitTrackerExecutor(ctx context.Context, prepared db.TrackerExecutionFence, provider, epoch string) (db.TrackerExecutionFence, error) {
	return s.trackerFenceSQL().AdmitExecutor(ctx, prepared, provider, epoch)
}
func (s *Store) AcknowledgeTrackerExecutor(ctx context.Context, checkpoint db.TrackerOwnerCheckpoint) error {
	return s.trackerFenceSQL().AcknowledgeExecutor(ctx, checkpoint)
}

func (s *Store) LoadTrackerExecutionHistory(ctx context.Context, root, project, role, write string) (db.TrackerExecutionFence, bool, error) {
	return s.trackerFenceSQL().LoadHistory(ctx, root, project, role, write)
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

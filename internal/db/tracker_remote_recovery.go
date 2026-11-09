package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/uid"
)

// TrackerRemoteRecoveryStore requires the exact retained owner evidence and current
// native project/transport transaction authority, never a provider credential.
// The daemon verifies provenance/root pins before invoking this interface.
type TrackerRemoteRecoveryStore interface {
	SettleTrackerOwnerNonSend(context.Context, TrackerExecutionFence) error
	AcceptTrackerOwnerResult(context.Context, TrackerExecutionFence) error
	RetireTrackerExecutorPreparation(context.Context, TrackerExecutionFence, string) (TrackerExecutionFence, error)
}

// TrackerOwnerProofMatchesReservation compares owner proof to its root attempt.
func TrackerOwnerProofMatchesReservation(root, owner TrackerExecutionFence) bool {
	return root.Validate() == nil && owner.Validate() == nil && root.Role == "coordinator" && owner.Role == "executor" && root.Remote != nil && owner.Remote != nil && *root.Remote == *owner.Remote && root.Connection == owner.Connection && root.RootInstanceUID == owner.RootInstanceUID && root.ProjectUID == owner.ProjectUID && root.WriteUID == owner.WriteUID && root.PendingEventUID == owner.PendingEventUID && root.SourceKey == owner.SourceKey && root.Locator == owner.Locator && root.Target == owner.Target
}
func loadTrackerHeadSQL(ctx context.Context, tx *sql.Tx, root, project, role string) (TrackerExecutionFence, bool, error) {
	key, err := TrackerExecutionFenceKey(root, project, role)
	if err != nil {
		return TrackerExecutionFence{}, false, err
	}
	return loadTrackerFenceKeySQL(ctx, tx, key)
}
func loadTrackerFenceKeySQL(ctx context.Context, tx *sql.Tx, key string) (TrackerExecutionFence, bool, error) {
	var f TrackerExecutionFence
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &f, json.RejectUnknownMembers(true)) != nil || f.Validate() != nil || !trackerImportKeyMatches(key, f) {
		return f, false, ErrTrackerFenceInvalid
	}
	return f, true, nil
}

// AcceptOwnerResult does not require that the old owner remain
// selected. It records only its already retained response; native exact-event
// application remains separate. No item readback can enter this transition.
func (a TrackerFenceSQL) AcceptOwnerResult(ctx context.Context, owner TrackerExecutionFence) error {
	if owner.Validate() != nil || owner.Role != "executor" || owner.Remote == nil || !owner.ProviderAcknowledged || (owner.Phase != TrackerWriteConfirmed && owner.Phase != TrackerWriteAcknowledged) || owner.RootInstanceUID != a.InstanceUID {
		return ErrTrackerFenceInvalid
	}
	return a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, owner.ProjectUID); err != nil {
			return err
		}
		root, found, err := loadTrackerHeadSQL(ctx, tx, owner.RootInstanceUID, owner.ProjectUID, "coordinator")
		if err != nil {
			return err
		}
		if !found || !TrackerOwnerProofMatchesReservation(root, owner) {
			return ErrTrackerFenceChanged
		}
		if root.Phase == TrackerWriteConfirmed || root.Phase == TrackerWriteAcknowledged {
			if bytes.Equal(root.Result, owner.Result) {
				return nil
			}
			return ErrTrackerFenceChanged
		}
		if root.Phase != TrackerWriteDispatching && root.Phase != TrackerWriteUnknown && root.Phase != TrackerWriteConvergedUnknown {
			return ErrTrackerOutcomeUnknown
		}
		var receipt TrackerStatusResult
		if json.Unmarshal(owner.Result, &receipt, json.RejectUnknownMembers(true)) != nil || receipt.Status != owner.Target || receipt.Version.IsZero() {
			return ErrTrackerFenceInvalid
		}
		next := root
		next.Phase = TrackerWriteConfirmed
		next.ProviderAcknowledged = true
		next.Result = bytes.Clone(owner.Result)
		return a.StoreInTransaction(ctx, tx, &root, next, root.ProcessEpoch)
	})
}

// SettleOwnerNonSend settles exact owner non-send evidence without dispatch.
func (a TrackerFenceSQL) SettleOwnerNonSend(ctx context.Context, owner TrackerExecutionFence) error {
	if owner.Validate() != nil || owner.Role != "executor" || owner.Remote == nil || owner.Phase != TrackerWriteNotSent || owner.ActivationRequired || owner.ProviderAcknowledged || owner.RootInstanceUID != a.InstanceUID || !bytes.Equal(owner.Result, []byte(`{"not_sent":true}`)) {
		return ErrTrackerFenceInvalid
	}
	return a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, owner.ProjectUID); err != nil {
			return err
		}
		root, found, err := loadTrackerHeadSQL(ctx, tx, owner.RootInstanceUID, owner.ProjectUID, "coordinator")
		if err != nil {
			return err
		}
		if !found {
			return ErrTrackerFenceChanged
		}
		isCurrent := TrackerOwnerProofMatchesReservation(root, owner)
		if !isCurrent {
			// The root may have retired preparation before owner preparation arrived,
			// then moved to a different owner. Only retained original reservation history
			// permits advancing this lane; never overwrite today's global hazard.
			history, _ := TrackerExecutionFenceKey(owner.RootInstanceUID, owner.ProjectUID, "coordinator")
			root, found, err = loadTrackerFenceKeySQL(ctx, tx, history+".history."+owner.WriteUID)
			if err != nil {
				return err
			}
			if !found || !TrackerOwnerProofMatchesReservation(root, owner) || root.Phase != TrackerWriteNotSent {
				return ErrTrackerFenceChanged
			}
			return advanceTrackerOwnerCheckpointSQL(ctx, tx, root)
		}
		if root.Phase == TrackerWriteNotSent {
			return advanceTrackerOwnerCheckpointSQL(ctx, tx, root)
		}
		if root.Phase != TrackerWritePrepared {
			return ErrTrackerOutcomeUnknown
		}
		next := root
		next.Phase = TrackerWriteNotSent
		next.ProviderAcknowledged = false
		next.Result = bytes.Clone(owner.Result)
		// Only a pre-dispatch root reservation can become a proven non-send.
		// Unknown or dispatching authority never becomes a retry grant.
		previousRaw, err := json.Marshal(root)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		key, _ := TrackerExecutionFenceKey(root.RootInstanceUID, root.ProjectUID, "coordinator")
		result, err := tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`, string(raw), key, string(previousRaw))
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrTrackerFenceChanged
		}
		if err := retainTrackerTerminalSQL(ctx, tx, next); err != nil {
			return err
		}
		return advanceTrackerOwnerCheckpointSQL(ctx, tx, next)
	})
}
func retainTrackerTerminalSQL(ctx context.Context, tx *sql.Tx, next TrackerExecutionFence) error {
	if next.Validate() != nil || !trackerFenceSettled(next) {
		return ErrTrackerFenceInvalid
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	key := trackerHistoryKey(next)
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, key, string(raw)); err != nil {
		return err
	}
	var actual string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&actual); err != nil {
		return err
	}
	if actual != string(raw) {
		return ErrTrackerFenceChanged
	}
	return nil
}

// RetireExecutorPreparation is invoked only after excluding the entire live
// attempt/finalizer. A new process also holds Kata's single-daemon lock. A
// restored prepared snapshot is unknown/inactive and never qualifies.
func (a TrackerFenceSQL) RetireExecutorPreparation(ctx context.Context, prepared TrackerExecutionFence, epoch string) (TrackerExecutionFence, error) {
	return a.retirePreparation(ctx, prepared, epoch, "executor")
}

// RetireCoordinatorPreparation retires an excluded root preparation.
func (a TrackerFenceSQL) RetireCoordinatorPreparation(ctx context.Context, prepared TrackerExecutionFence, epoch string) (TrackerExecutionFence, error) {
	return a.retirePreparation(ctx, prepared, epoch, "coordinator")
}

// TrackerCoordinatorRetirementStore retires excluded root preparations.
type TrackerCoordinatorRetirementStore interface {
	RetireTrackerCoordinatorPreparation(context.Context, TrackerExecutionFence, string) (TrackerExecutionFence, error)
}

func (a TrackerFenceSQL) retirePreparation(ctx context.Context, prepared TrackerExecutionFence, epoch, role string) (TrackerExecutionFence, error) {
	next := prepared
	if prepared.Validate() != nil || prepared.Role != role || (role != "executor" && role != "coordinator") || prepared.Remote == nil || prepared.Phase != TrackerWritePrepared || prepared.ActivationRequired || !a.local(prepared) || !uid.Valid(epoch) {
		return next, ErrTrackerOutcomeUnknown
	}
	next.Phase = TrackerWriteNotSent
	next.Result = []byte(`{"not_sent":true}`)
	err := a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, prepared.ProjectUID); err != nil {
			return err
		}
		if role == "coordinator" {
			var spoke int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_bindings f JOIN projects p ON p.id=f.project_id WHERE p.uid=$1 AND f.role='spoke' AND f.enabled=1`, prepared.ProjectUID).Scan(&spoke); err != nil {
				return err
			}
			if spoke != 0 {
				return ErrIssueSyncFederationBinding
			}
		}
		if blocked, err := trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, prepared.ProjectUID); err != nil {
			return err
		} else if blocked {
			return ErrTrackerOutcomeUnknown
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		expected, err := json.Marshal(prepared)
		if err != nil {
			return err
		}
		key, _ := TrackerExecutionFenceKey(prepared.RootInstanceUID, prepared.ProjectUID, role)
		result, err := tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`, string(raw), key, string(expected))
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrTrackerFenceChanged
		}
		return retainTrackerTerminalSQL(ctx, tx, next)
	})
	return next, err
}

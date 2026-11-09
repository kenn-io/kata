package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/tracker"
)

// TrackerRemoteRetryPreparation carries the exact witnessed owner terminal and
// root reservation. Transport authentication and invocation exclusion are
// required separately; these values alone never authorize a provider call.
type TrackerRemoteRetryPreparation struct {
	PriorOwner  TrackerExecutionFence
	Reservation TrackerExecutionFence
}

// TrackerStatusRetryStore prepares a root retry under native status authority.
type TrackerStatusRetryStore interface {
	PrepareTrackerStatusRetry(context.Context, TrackerStatusAdmission, TrackerRemoteRetryPreparation) (TrackerExecutionFence, error)
}

// TrackerExecutorRetryStore prepares the same retry in the owner lane.
type TrackerExecutorRetryStore interface {
	PrepareTrackerExecutorRetry(context.Context, TrackerRemoteRetryPreparation, string, string) (TrackerExecutionFence, error)
}

func validateRemoteRetryPair(p TrackerRemoteRetryPreparation) (TrackerExecutionFence, error) {
	owner, next := p.PriorOwner, p.Reservation
	if owner.Validate() != nil || owner.Role != "executor" || owner.Remote == nil || owner.Phase != TrackerWriteNotSent || owner.ActivationRequired || owner.ProviderAcknowledged || !bytes.Equal(owner.Result, []byte(`{"not_sent":true}`)) || next.Validate() != nil || next.Role != "coordinator" || next.Remote == nil || next.Phase != TrackerWritePrepared || next.ActivationRequired || next.ProviderAcknowledged || len(next.Result) != 0 {
		return TrackerExecutionFence{}, ErrTrackerOutcomeUnknown
	}
	root := owner
	root.Role = "coordinator"
	root.Sequence = owner.Remote.RootSequence
	root.PreviousHead = owner.Remote.RootPreviousHead
	root.ProcessEpoch = owner.Remote.RootEpoch
	if root.Validate() != nil || !TrackerOwnerProofMatchesReservation(root, owner) || next.RootInstanceUID != owner.RootInstanceUID || next.ProjectUID != owner.ProjectUID || next.Connection != owner.Connection || next.WriteUID != owner.WriteUID || next.PendingEventUID != owner.PendingEventUID || next.SourceKey != owner.SourceKey || next.Locator != owner.Locator || next.Target != owner.Target || next.ProcessEpoch != root.ProcessEpoch || next.Sequence != root.Sequence+1 || next.PreviousHead != TrackerFenceHead(root) || next.Remote.OwnerSequence != owner.Sequence+1 || next.Remote.OwnerPreviousHead != TrackerFenceHead(owner) || next.Remote.UID == owner.Remote.UID || next.Remote.ArgumentsDigest != owner.Remote.ArgumentsDigest {
		return TrackerExecutionFence{}, ErrTrackerOutcomeUnknown
	}
	return root, nil
}

// archiveRemoteRetryInTransaction is private to the two fully checked native
// admission paths. Every mutation rolls back with the caller's transaction.
func (a TrackerFenceSQL) archiveRemoteRetryInTransaction(ctx context.Context, tx *sql.Tx, previous, next TrackerExecutionFence) error {
	key, err := TrackerExecutionFenceKey(previous.RootInstanceUID, previous.ProjectUID, previous.Role)
	if err != nil || !a.local(previous) || !a.local(next) {
		return ErrTrackerFenceInvalid
	}
	oldRaw, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	newRaw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	history := trackerHistoryKey(previous)
	var storedHead, storedHistory string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&storedHead); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTrackerOutcomeUnknown
		}
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, history).Scan(&storedHistory); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTrackerOutcomeUnknown
		}
		return err
	}
	if storedHead != string(oldRaw) || storedHistory != string(oldRaw) {
		return ErrTrackerOutcomeUnknown
	}
	archive := trackerAttemptKey(previous)
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`, archive, storedHistory); err != nil {
		return err
	}
	var archived string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, archive).Scan(&archived); err != nil {
		return err
	}
	if archived != storedHistory {
		return ErrTrackerOutcomeUnknown
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1 AND value=$2`, history, storedHistory)
	if err != nil {
		return err
	}
	if n, err := deleted.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrTrackerFenceChanged
	}
	updated, err := tx.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2 AND value=$3`, string(newRaw), key, string(oldRaw))
	if err != nil {
		return err
	}
	if n, err := updated.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrTrackerFenceChanged
	}
	return nil
}

func checkpointMatchesRetry(c TrackerOwnerCheckpoint, owner TrackerExecutionFence) bool {
	return !c.Restored && c.Validate() == nil && c.RootInstanceUID == owner.RootInstanceUID && c.ProjectUID == owner.ProjectUID && c.OwnerInstanceUID == owner.Connection.OwnerInstanceUID && c.Selection == owner.Connection && c.Sequence == owner.Sequence && c.Head == TrackerFenceHead(owner) && c.Attempt == *owner.Remote && c.WriteUID == owner.WriteUID && c.Disposition == TrackerWriteNotSent && c.ResultDigest == tracker.WorkerArgumentsDigest(owner.Result)
}

// PrepareExecutorRetry checks the owner-local authority and exact prior proof
// in one transaction. Its caller separately authenticates the root reservation
// and holds the invocation exclusion through the provider finalizer.
func (a TrackerFenceSQL) PrepareExecutorRetry(ctx context.Context, p TrackerRemoteRetryPreparation, provider, epoch string) (TrackerExecutionFence, error) {
	if _, err := validateRemoteRetryPair(p); err != nil || p.PriorOwner.ProcessEpoch != epoch || !a.local(p.PriorOwner) {
		return TrackerExecutionFence{}, ErrTrackerOutcomeUnknown
	}
	prepared, err := TrackerExecutorPreparation(p.Reservation, epoch)
	if err != nil || !a.local(prepared) {
		return TrackerExecutionFence{}, ErrTrackerFenceInvalid
	}
	err = a.Transact(ctx, func(tx *sql.Tx) error {
		if err := a.authorize(ctx, tx, prepared.ProjectUID); err != nil {
			return err
		}
		if blocked, err := trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, prepared.ProjectUID); err != nil {
			return err
		} else if blocked {
			return ErrTrackerOutcomeUnknown
		}
		if err := a.ValidateTrackerRegistryInTransaction(ctx, tx, prepared.Connection, provider, epoch); err != nil {
			return err
		}
		return a.archiveRemoteRetryInTransaction(ctx, tx, p.PriorOwner, prepared)
	})
	return prepared, err
}

package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/kata/internal/uid"
)

type TrackerStatusAdmission struct {
	RemoteAttempt       *TrackerRemoteAttempt
	ExpectedReservation *TrackerExecutionFence
	Guard               IssueSyncImportGuard
	MappingID           int64
	ExternalID          string
	IssueUID            string
	PendingEventUID     string
	Target              string
	Connection          IssueSyncConnection
	ProcessEpoch        string
	// Now comes from the executing daemon's clock, not a control request body.
	Now time.Time
}
type TrackerStatusStore interface {
	AdmitTrackerStatusWrite(context.Context, TrackerStatusAdmission) (TrackerExecutionFence, error)
}

// StoreInTransaction composes the same validated CAS with native domain checks.
// The caller owns tx and its commit; this never opens a nested transaction.
func (a TrackerFenceSQL) StoreInTransaction(ctx context.Context, tx *sql.Tx, previous *TrackerExecutionFence, next TrackerExecutionFence, epoch string) error {
	a.Transact = func(ctx context.Context, run func(*sql.Tx) error) error { return run(tx) }
	return a.Store(ctx, previous, next, epoch)
}

// AdmitStatusInTransaction receives native claim-fenced binding/mapping reads.
// Live policy transaction fences are installed by native BeginTx; every further
// root/selection/activation/event check and the dispatch record share that tx.
func (a TrackerFenceSQL) AdmitStatusInTransaction(ctx context.Context, tx *sql.Tx, binding IssueSyncBinding, mapping IssueStatusMapping, p TrackerStatusAdmission) (TrackerExecutionFence, error) {
	if p.Guard.BindingUpdatedAt == nil || p.Guard.BindingID != binding.ID || p.MappingID != mapping.Mapping.ID || p.ExternalID != mapping.Mapping.ExternalID || mapping.Mapping.IssueID == nil || mapping.PendingEvent == nil || mapping.State.PendingEventUID != p.PendingEventUID || mapping.PendingEvent.UID != p.PendingEventUID || !uid.Valid(p.ProcessEpoch) || p.Now.IsZero() || p.Now.Before(p.Guard.StartedAt) || !p.Now.Before(p.Guard.StartedAt.Add(25*time.Minute)) {
		return TrackerExecutionFence{}, ErrIssueSyncBindingChanged
	}
	if p.Target != "open" && p.Target != "closed" {
		return TrackerExecutionFence{}, ErrTrackerFenceInvalid
	}
	if (mapping.PendingEvent.Type == "issue.closed") != (p.Target == "closed") || (mapping.PendingEvent.Type != "issue.closed" && mapping.PendingEvent.Type != "issue.reopened") {
		return TrackerExecutionFence{}, ErrIssueSyncBindingChanged
	}
	selection, err := IssueSyncConnectionFromConfig(binding.Config)
	if err != nil {
		return TrackerExecutionFence{}, err
	}
	if selection == nil || *selection != p.Connection {
		return TrackerExecutionFence{}, ErrIssueSyncBindingChanged
	}
	if selection.OwnerInstanceUID != a.InstanceUID && p.RemoteAttempt == nil {
		return TrackerExecutionFence{}, ErrTrackerFenceInvalid
	}
	if selection.OwnerInstanceUID == a.InstanceUID && (p.RemoteAttempt != nil || p.ExpectedReservation != nil) {
		return TrackerExecutionFence{}, ErrTrackerFenceInvalid
	}
	if selection.OwnerInstanceUID == a.InstanceUID {
		if err := a.ValidateTrackerRegistryInTransaction(ctx, tx, *selection, binding.Provider, p.ProcessEpoch); err != nil {
			return TrackerExecutionFence{}, err
		}
	}
	mode, err := IssueStatusMode(binding.Config)
	if err != nil {
		return TrackerExecutionFence{}, err
	}
	if mode != "two-way" {
		return TrackerExecutionFence{}, ErrIssueSyncNotEnabled
	}
	var projectUID, issueUID string
	if err := tx.QueryRowContext(ctx, `SELECT p.uid,i.uid FROM projects p JOIN issues i ON i.project_id=p.id WHERE i.id=$1 AND p.id=$2 AND p.deleted_at IS NULL AND i.deleted_at IS NULL`, *mapping.Mapping.IssueID, binding.ProjectID).Scan(&projectUID, &issueUID); err != nil {
		return TrackerExecutionFence{}, err
	}
	if issueUID != p.IssueUID {
		return TrackerExecutionFence{}, ErrIssueSyncBindingChanged
	}
	if err := a.authorize(ctx, tx, projectUID); err != nil {
		return TrackerExecutionFence{}, err
	}
	if blocked, err := trackerRestoreBarrierPresentSQL(ctx, tx, a.InstanceUID, projectUID); err != nil {
		return TrackerExecutionFence{}, err
	} else if blocked {
		return TrackerExecutionFence{}, ErrTrackerOutcomeUnknown
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, TrackerActivationKey(a.InstanceUID, projectUID)).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TrackerExecutionFence{}, ErrTrackerActivationRequired
		}
		return TrackerExecutionFence{}, err
	}
	var active TrackerActivation
	if len(raw) > 8192 || json.Unmarshal([]byte(raw), &active, json.RejectUnknownMembers(true)) != nil || active.RootInstanceUID != a.InstanceUID || active.ProjectUID != projectUID || !active.Allows(p.ProcessEpoch, binding.ID, *selection, binding.SourceKey, mapping.PendingEvent.ID) {
		return TrackerExecutionFence{}, ErrTrackerActivationRequired
	}
	key, _ := TrackerExecutionFenceKey(a.InstanceUID, projectUID, "coordinator")
	var previous *TrackerExecutionFence
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw)
	if err == nil {
		var f TrackerExecutionFence
		if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &f, json.RejectUnknownMembers(true)) != nil || f.Validate() != nil || !trackerKeyMatches(key, f) {
			return TrackerExecutionFence{}, ErrTrackerFenceInvalid
		}
		if p.ExpectedReservation != nil {
			expected, marshalErr := json.Marshal(p.ExpectedReservation)
			actual, actualErr := json.Marshal(f)
			if marshalErr != nil || actualErr != nil || !bytes.Equal(expected, actual) || f.Remote == nil || f.Phase != TrackerWritePrepared || !f.CanDispatch(p.ProcessEpoch) || f.WriteUID != mustTrackerStatusIdentity(a.InstanceUID, projectUID, binding.SourceKey, mapping.Mapping.ExternalID, p.PendingEventUID, p.Target) || f.Connection != p.Connection || p.RemoteAttempt.UID != f.Remote.UID || p.RemoteAttempt.ArgumentsDigest != f.Remote.ArgumentsDigest || p.RemoteAttempt.OwnerEpoch != f.Remote.OwnerEpoch {
				return TrackerExecutionFence{}, ErrTrackerFenceChanged
			}
			if err := validateTrackerOwnerPredecessorSQL(ctx, tx, f); err != nil {
				return TrackerExecutionFence{}, err
			}
			dispatched := f
			dispatched.Phase = TrackerWriteDispatching
			if err := a.StoreInTransaction(ctx, tx, &f, dispatched, p.ProcessEpoch); err != nil {
				return TrackerExecutionFence{}, err
			}
			return dispatched, nil
		}
		if !trackerFenceSettled(f) || f.ActivationRequired {
			return TrackerExecutionFence{}, ErrTrackerOutcomeUnknown
		}
		previous = &f
	} else if !errors.Is(err, sql.ErrNoRows) {
		return TrackerExecutionFence{}, err
	}
	f := TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: a.InstanceUID, ProjectUID: projectUID, Connection: *selection, Sequence: 1, PendingEventUID: p.PendingEventUID, SourceKey: binding.SourceKey, Locator: mapping.Mapping.ExternalID, Target: p.Target, ProcessEpoch: p.ProcessEpoch, Phase: TrackerWritePrepared}
	if previous != nil {
		f.Sequence = previous.Sequence + 1
		f.PreviousHead = TrackerFenceHead(*previous)
	}
	if p.ExpectedReservation != nil {
		return TrackerExecutionFence{}, ErrTrackerFenceChanged
	}
	if p.RemoteAttempt != nil {
		remote := *p.RemoteAttempt
		remote.RootSequence = f.Sequence
		remote.RootPreviousHead = f.PreviousHead
		remote.RootEpoch = p.ProcessEpoch
		f.Remote = &remote
		if err := validateTrackerOwnerPredecessorSQL(ctx, tx, f); err != nil {
			return TrackerExecutionFence{}, err
		}
	}
	f.WriteUID, err = TrackerStatusWriteUID(f.RootInstanceUID, f.ProjectUID, f.SourceKey, f.Locator, f.PendingEventUID, f.Target)
	if err != nil {
		return TrackerExecutionFence{}, err
	}
	if err := a.StoreInTransaction(ctx, tx, previous, f, p.ProcessEpoch); err != nil {
		return TrackerExecutionFence{}, err
	}
	if f.Remote != nil {
		return f, nil
	}
	dispatched := f
	dispatched.Phase = TrackerWriteDispatching
	if err := a.StoreInTransaction(ctx, tx, &f, dispatched, p.ProcessEpoch); err != nil {
		return TrackerExecutionFence{}, err
	}
	return dispatched, nil
}

func mustTrackerStatusIdentity(root, project, source, locator, event, target string) string {
	identity, _ := TrackerStatusWriteUID(root, project, source, locator, event, target)
	return identity
}

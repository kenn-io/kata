package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

type trackerStatusAttemptKey struct{}
type trackerStatusAttempt struct {
	runtime        *TrackerRuntime
	source, target string
}

var _ issuesync.NamedStatusCoordinator = (*TrackerRuntime)(nil)

// WriteNamedStatus is the root-local coordinator. Read-before-dispatch no-ops
// create no acknowledgement claim; admitted outcomes persist before returning.
// Unknowns never become fresh writes through claim expiry or another caller.
func (r *TrackerRuntime) WriteNamedStatus(ctx context.Context, binding db.IssueSyncBinding, m db.IssueStatusMapping, target string, guard db.IssueSyncImportGuard, send issuesync.NamedStatusSend, admit func(context.Context) error) (issuesync.StatusObservation, error) {
	if send == nil || admit == nil || m.PendingEvent == nil || m.Mapping.IssueID == nil {
		return issuesync.StatusObservation{}, db.ErrTrackerFenceInvalid
	}
	if _, err := r.selectedConnection(ctx, binding.Provider, tracker.ActionStatus); err != nil {
		return issuesync.StatusObservation{}, err
	}
	authority, ok := ctx.Value(trackerRunAuthorityKey{}).(trackerRunAuthority)
	if !ok || authority.principal.Kind != tracker.SubjectService || authority.root != r.store.InstanceUID() || authority.source != binding.SourceKey {
		return issuesync.StatusObservation{}, tracker.ErrDenied
	}
	selection, err := db.IssueSyncConnectionFromConfig(binding.Config)
	if err != nil || selection == nil {
		return issuesync.StatusObservation{}, db.ErrTrackerFenceInvalid
	}
	fences, ok := r.store.(db.TrackerFenceStore)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	admissions, ok := r.store.(db.TrackerStatusStore)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	activation, ok := r.store.(db.TrackerActivationStore)
	if !ok {
		return issuesync.StatusObservation{}, tracker.ErrUnavailable
	}
	identity, err := db.TrackerStatusWriteUID(authority.root, authority.project, binding.SourceKey, m.Mapping.ExternalID, m.PendingEvent.UID, target)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	previous, found, err := fences.LoadTrackerExecutionFence(ctx, authority.root, authority.project, "coordinator")
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	if found && previous.Phase == db.TrackerWriteConfirmed && previous.WriteUID == identity && previous.Connection == *selection {
		var receipt db.TrackerStatusResult
		if json.Unmarshal(previous.Result, &receipt, json.RejectUnknownMembers(true)) != nil || receipt.Version.IsZero() || receipt.Status != target {
			return issuesync.StatusObservation{}, db.ErrTrackerFenceInvalid
		}
		return trackerObservationFromReceipt(receipt, identity), nil
	}
	if found && previous.Phase != db.TrackerWriteAcknowledged && previous.Phase != db.TrackerWriteNotSent {
		return issuesync.StatusObservation{}, trackerUnknownOutcome(nil)
	}
	active, found, err := activation.LoadTrackerActivation(ctx, authority.project)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	if !found || !active.Allows(r.epoch, binding.ID, *selection, binding.SourceKey, m.PendingEvent.ID) {
		return issuesync.StatusObservation{}, db.ErrTrackerActivationRequired
	}
	issue, err := r.store.IssueByID(ctx, *m.Mapping.IssueID)
	if err != nil {
		return issuesync.StatusObservation{}, err
	}
	attemptCtx := context.WithValue(ctx, trackerStatusAttemptKey{}, trackerStatusAttempt{runtime: r, source: binding.SourceKey, target: target})
	if selection.OwnerInstanceUID != r.store.InstanceUID() {
		metadata := trackerRemoteStatus{identity: identity, originalAdmit: admit, admission: db.TrackerStatusAdmission{Guard: guard, MappingID: m.Mapping.ID, ExternalID: m.Mapping.ExternalID, IssueUID: issue.UID, PendingEventUID: m.PendingEvent.UID, Target: target, Connection: *selection, ProcessEpoch: r.epoch}}
		return send(context.WithValue(attemptCtx, trackerRemoteStatusKey{}, metadata), func() error { return admit(attemptCtx) })
	}
	var dispatched *db.TrackerExecutionFence
	callbackUsed := false
	obs, callErr := send(attemptCtx, func() error {
		if callbackUsed {
			return db.ErrTrackerOutcomeUnknown
		}
		callbackUsed = true
		if err := admit(ctx); err != nil {
			return err
		}
		if _, err := r.selectedConnection(ctx, binding.Provider, tracker.ActionStatus); err != nil {
			return err
		}
		var fence db.TrackerExecutionFence
		err := r.withSnapshotAdmission(ctx, func() error {
			var err error
			fence, err = admissions.AdmitTrackerStatusWrite(ctx, db.TrackerStatusAdmission{Guard: guard, MappingID: m.Mapping.ID, ExternalID: m.Mapping.ExternalID, IssueUID: issue.UID, PendingEventUID: m.PendingEvent.UID, Target: target, Connection: *selection, ProcessEpoch: r.epoch, Now: time.Now().UTC()})
			return err
		})
		if err != nil {
			return err
		}
		dispatched = &fence
		return nil
	})
	// Provider observations cannot supply root execution authority.
	obs.TrackerWriteUID = ""
	// Only GitHub records an additional API locator during observation. Other
	// providers already identify the item through the immutable external_id;
	// retaining their readback locator would make the receipt unacknowledgeable.
	if binding.Provider != "github" {
		obs.Locator = ""
	}
	if dispatched == nil {
		return obs, callErr
	}
	// Retaining an admitted result must outlive caller cancellation. The bounded
	// cleanup retains all project/host fences; inability to persist stays unknown.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	next := *dispatched
	if callErr != nil || obs.Status != target || obs.Version.IsZero() {
		next.Phase = db.TrackerWriteUnknown
		persistErr := fences.StoreTrackerExecutionFence(finalCtx, dispatched, next, r.epoch)
		return issuesync.StatusObservation{}, trackerUnknownOutcome(errors.Join(trackerClientError(callErr), persistErr))
	}
	receipt := db.TrackerStatusResult{Raw: obs.RawStatus, Status: obs.Status, ClosedReason: obs.ClosedReason, ClosedAt: obs.ClosedAt, Version: obs.Version, Locator: obs.Locator}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > 16<<10 {
		next.Phase = db.TrackerWriteUnknown
		persistErr := fences.StoreTrackerExecutionFence(finalCtx, dispatched, next, r.epoch)
		return issuesync.StatusObservation{}, trackerUnknownOutcome(persistErr)
	}
	next.Phase = db.TrackerWriteConfirmed
	next.ProviderAcknowledged = true
	next.Result = raw
	if err := fences.StoreTrackerExecutionFence(finalCtx, dispatched, next, r.epoch); err != nil {
		return issuesync.StatusObservation{}, trackerUnknownOutcome(err)
	}
	obs.TrackerWriteUID = identity
	return obs, nil
}
func trackerObservationFromReceipt(r db.TrackerStatusResult, writeUID string) issuesync.StatusObservation {
	return issuesync.StatusObservation{RawStatus: r.Raw, Status: r.Status, ClosedReason: r.ClosedReason, ClosedAt: r.ClosedAt, Version: r.Version, Locator: r.Locator, TrackerWriteUID: writeUID}
}

func trackerUnknownOutcome(cause error) error {
	return &issuesync.StatusError{Message: db.ErrTrackerOutcomeUnknown.Error(), Ambiguous: true, Cause: errors.Join(db.ErrTrackerOutcomeUnknown, cause)}
}

// RecoverNamedStatus consumes only a retained original completed response. It
// does not read the provider, rewrite observation clocks or clear newer intent.
func (r *TrackerRuntime) RecoverNamedStatus(ctx context.Context, binding db.IssueSyncBinding, guard db.IssueSyncImportGuard) error {
	selection, err := db.IssueSyncConnectionFromConfig(binding.Config)
	if err != nil {
		return err
	}
	if selection != nil && selection.OwnerInstanceUID != r.store.InstanceUID() {
		project, err := r.store.ProjectByID(ctx, binding.ProjectID)
		if err != nil {
			return err
		}
		if err := r.recoverRemoteOwner(ctx, project.UID, binding.Provider, *selection); err != nil {
			return err
		}
	}

	if _, err := r.selectedConnection(ctx, binding.Provider, tracker.ActionRead); err != nil {
		return err
	}
	receipts, ok := r.store.(db.TrackerStatusReceiptStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	return receipts.AcknowledgeRetainedTrackerStatus(ctx, guard)
}

package daemon

import (
	"context"
	"encoding/json/v2"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

func (r *TrackerRuntime) ownerWorkerStage(ctx context.Context, cfg ServerConfig, w *trackerOwnerWorker, t tracker.WorkerTicket, stage string, owner *db.TrackerExecutionFence) (trackerWorkerDecision, error) {
	project, err := r.store.ProjectByUID(ctx, t.ProjectUID)
	if err != nil {
		return trackerWorkerDecision{}, err
	}
	binding, err := r.store.FederationBindingByProject(ctx, project.ID)
	if err != nil || binding.RelayConfig == nil {
		return trackerWorkerDecision{}, tracker.ErrUnavailable
	}
	presence, _ := r.workerPresence(ctx, project, binding, w)
	presence.Connections = nil
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := forwardTrackerWorker(requestCtx, cfg, project, trackerWorkerBody{Presence: presence, Signal: &trackerWorkerSignal{CallUID: t.CallUID, SessionUID: t.SessionUID, Stage: stage, Owner: owner}}, "admit")
	if err != nil || result.Decision == nil {
		return trackerWorkerDecision{}, tracker.ErrUnavailable
	}
	return *result.Decision, nil
}
func (r *TrackerRuntime) ownerExecutionContext(ctx context.Context, t tracker.WorkerTicket, pin db.RootKeyPin) (context.Context, error) {
	project, err := r.store.ProjectByUID(ctx, t.ProjectUID)
	if err != nil {
		return ctx, err
	}
	binding, err := r.store.FederationBindingByProject(ctx, project.ID)
	if err != nil || !binding.Enabled || binding.Role != db.FederationRoleSpoke || binding.RelayConfig == nil || binding.RelayConfig.AuthorityUID != t.RootInstanceUID {
		return ctx, tracker.ErrUnavailable
	}
	return db.WithAdditionalTransactionFence(ctx, db.TrackerExecutorTransportFence(r.store.InstanceUID(), t.ProjectUID, binding, pin)), nil
}
func (r *TrackerRuntime) executeOwnerStatus(ctx context.Context, cfg ServerConfig, w *trackerOwnerWorker, session *trackerOwnerSession, c trackerLocalConnection, t tracker.WorkerTicket, args trackerWorkerArguments) (trackerWorkerStatusOutcome, error) {
	empty := trackerWorkerStatusOutcome{}
	if t.Principal.Kind != tracker.SubjectService || t.SourceKey == "" || args.Target == "" || workerArgumentsExternalID(t.Provider, args) == "" {
		return empty, tracker.ErrDenied
	}
	executor, ok := r.store.(db.TrackerExecutorStore)
	if !ok {
		return empty, tracker.ErrUnavailable
	}
	fences, ok := r.store.(db.TrackerFenceStore)
	if !ok {
		return empty, tracker.ErrUnavailable
	}
	var prepared, dispatched *db.TrackerExecutionFence
	callbackUsed := false
	value, callErr := session.provider.providerCall(ctx, c, t, args, func() error {
		if callbackUsed {
			return db.ErrTrackerOutcomeUnknown
		}
		callbackUsed = true
		if _, err := r.selectedConnection(ctx, t.Provider, tracker.ActionStatus); err != nil {
			return err
		}
		reserve, err := r.ownerWorkerStage(ctx, cfg, w, t, "prepare", nil)
		if err != nil {
			return err
		}
		root := reserve.Reservation
		if root.Validate() != nil || root.RootInstanceUID != t.RootInstanceUID || root.ProjectUID != t.ProjectUID || root.Connection != t.Selection || root.SourceKey != t.SourceKey || root.Locator != workerArgumentsExternalID(t.Provider, args) || root.Target != args.Target || root.Remote == nil || root.Remote.UID != t.CallUID || root.Remote.ArgumentsDigest != t.ArgumentsDigest || root.Remote.OwnerEpoch != t.OwnerEpoch {
			return db.ErrTrackerFenceChanged
		}
		digest, err := workerReservationDigest(root)
		if err != nil {
			return err
		}
		pin, err := r.store.RootAuthority(ctx, t.ProjectUID)
		if err != nil {
			return err
		}
		if err := verifyWorkerContinuation(reserve.Authority, t, pin, tracker.WorkerReserve, root.WriteUID, digest); err != nil {
			return err
		}
		nativeCtx, err := r.ownerExecutionContext(ctx, t, pin)
		if err != nil {
			return err
		}
		var owner db.TrackerExecutionFence
		if err := r.withSnapshotAdmission(nativeCtx, func() error {
			var err error
			owner, err = executor.PrepareTrackerExecutor(nativeCtx, root, t.Provider, r.epoch)
			return err
		}); err != nil {
			return err
		}
		prepared = &owner
		permit, err := r.ownerWorkerStage(ctx, cfg, w, t, "permit", prepared)
		if err != nil {
			return err
		}
		if !workerFenceEqual(permit.Reservation, root) {
			return db.ErrTrackerFenceChanged
		}
		digest, err = workerPermitDigest(root, owner)
		if err != nil {
			return err
		}
		pin, err = r.store.RootAuthority(ctx, t.ProjectUID)
		if err != nil {
			return err
		}
		if err := verifyWorkerContinuation(permit.Authority, t, pin, tracker.WorkerPermit, root.WriteUID, digest); err != nil {
			return err
		}
		if _, err := r.selectedConnection(ctx, t.Provider, tracker.ActionStatus); err != nil {
			return err
		}
		nativeCtx, err = r.ownerExecutionContext(ctx, t, pin)
		if err != nil {
			return err
		}
		var sent db.TrackerExecutionFence
		if err := r.withSnapshotAdmission(nativeCtx, func() error {
			if time.Now().Unix() >= permit.Authority.Ticket.ExpiresAt {
				return tracker.ErrUnavailable
			}
			var err error
			sent, err = executor.AdmitTrackerExecutor(nativeCtx, owner, t.Provider, r.epoch)
			return err
		}); err != nil {
			return err
		}
		dispatched = &sent
		return nil
	})
	observation, valid := value.(issuesync.StatusObservation)
	observation.TrackerWriteUID = ""
	if t.Provider != "github" {
		observation.Locator = ""
	}
	if prepared == nil {
		return trackerWorkerStatusOutcome{Observation: observation}, callErr
	}
	// Finalization must outlive caller cancellation. Preserve native project
	// policy; a concurrent revocation can still prevent retention and leaves an
	// unknown hazard. The session remains busy through this entire finalizer.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if dispatched == nil {
		recovery, ok := r.store.(db.TrackerRemoteRecoveryStore)
		if !ok {
			return empty, trackerUnknownOutcome(nil)
		}
		proof, err := recovery.RetireTrackerExecutorPreparation(finalCtx, *prepared, r.epoch)
		if err == nil {
			_ = r.pushOwnerTerminal(finalCtx, cfg, w, t, proof)
			return trackerWorkerStatusOutcome{Proof: &proof}, nil
		}
		return empty, trackerUnknownOutcome(err)
	}
	next := *dispatched
	if callErr != nil || !valid || observation.Status != args.Target || observation.Version.IsZero() {
		next.Phase = db.TrackerWriteUnknown
		_ = fences.StoreTrackerExecutionFence(finalCtx, dispatched, next, r.epoch)
		return empty, trackerUnknownOutcome(nil)
	}
	receipt := db.TrackerStatusResult{Raw: observation.RawStatus, Status: observation.Status, ClosedReason: observation.ClosedReason, ClosedAt: observation.ClosedAt, Version: observation.Version, Locator: observation.Locator}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > 16<<10 {
		next.Phase = db.TrackerWriteUnknown
		_ = fences.StoreTrackerExecutionFence(finalCtx, dispatched, next, r.epoch)
		return empty, trackerUnknownOutcome(nil)
	}
	next.Phase = db.TrackerWriteConfirmed
	next.ProviderAcknowledged = true
	next.Result = raw
	if err := fences.StoreTrackerExecutionFence(finalCtx, dispatched, next, r.epoch); err != nil {
		return empty, trackerUnknownOutcome(err)
	}
	_ = r.pushOwnerTerminal(finalCtx, cfg, w, t, next)
	return trackerWorkerStatusOutcome{Observation: observation, Proof: &next}, nil
}

// Terminal delivery uses the original exact owner proof. It is metadata-only;
// failure never resends a provider request or deletes the retained owner head.
func (r *TrackerRuntime) pushOwnerTerminal(ctx context.Context, cfg ServerConfig, w *trackerOwnerWorker, t tracker.WorkerTicket, proof db.TrackerExecutionFence) error {
	project, err := r.store.ProjectByUID(ctx, t.ProjectUID)
	if err != nil {
		return err
	}
	binding, err := r.store.FederationBindingByProject(ctx, project.ID)
	if err != nil || binding.RelayConfig == nil {
		return tracker.ErrUnavailable
	}
	presence, _ := r.workerPresence(ctx, project, binding, w)
	presence.Connections = nil
	_, err = forwardTrackerWorker(ctx, cfg, project, trackerWorkerBody{Presence: presence, Signal: &trackerWorkerSignal{CallUID: t.CallUID, SessionUID: t.SessionUID, Stage: "terminal", Owner: &proof}}, "metadata")
	return err
}

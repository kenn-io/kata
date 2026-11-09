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

type trackerWorkerStatusOutcome struct {
	Observation issuesync.StatusObservation `json:"observation"`
	Proof       *db.TrackerExecutionFence   `json:"proof,omitempty"`
}

func (r *trackerRemoteRun) writeStatus(ctx context.Context, args trackerWorkerArguments, admit func() error) (issuesync.StatusObservation, error) {
	empty := issuesync.StatusObservation{}
	metadata, ok := ctx.Value(trackerRemoteStatusKey{}).(trackerRemoteStatus)
	if !ok || metadata.originalAdmit == nil || r == nil || r.runtime == nil || admit == nil || metadata.admission.Connection != r.descriptor.Selection || metadata.admission.Target != args.Target || metadata.admission.ExternalID != workerArgumentsExternalID(r.descriptor.Provider, args) || !tracker.DigestValid(metadata.identity) {
		return empty, tracker.ErrUnavailable
	}
	if err := r.runtime.acknowledgeRemoteCheckpoint(ctx, r.project, r.descriptor.Provider, r.descriptor.Selection.OwnerInstanceUID); err != nil {
		return empty, err
	}
	raw, err := json.Marshal(args, json.Deterministic(true))
	if err != nil || len(raw) > 48<<10 {
		return empty, tracker.ErrInvalid
	}
	signed, err := r.ticket(ctx, "write-status", tracker.ActionStatus, raw)
	if err != nil {
		return empty, err
	}
	ctx, cancel := trackerWorkerReplyContext(ctx, signed.Ticket)
	defer cancel()
	state := &trackerRootWorkerWrite{ctx: ctx, runtime: r.runtime, ticket: signed.Ticket, admission: metadata.admission, originalAdmit: metadata.originalAdmit}
	pending, err := r.runtime.workerBroker.enqueue(ctx, trackerWorkerCommand{Ticket: signed, Arguments: raw}, false, state)
	if err != nil {
		return empty, err
	}
	defer r.runtime.workerBroker.release(signed.Ticket.CallUID)
	settled := false
	defer func() {
		if !settled {
			state.retainUnsettled()
		}
	}()
	var reply trackerWorkerReply
	select {
	case <-ctx.Done():
		state.mu.Lock()
		dispatched := state.dispatched != nil
		state.mu.Unlock()
		if dispatched {
			return empty, trackerUnknownOutcome(ctx.Err())
		}
		return empty, ctx.Err()
	case reply = <-pending.result:
	}
	if reply.Code != "ok" {
		state.mu.Lock()
		dispatched := state.dispatched != nil
		state.mu.Unlock()
		if dispatched {
			return empty, trackerUnknownOutcome(trackerWorkerError(reply))
		}
		return empty, trackerWorkerError(reply)
	}
	var outcome trackerWorkerStatusOutcome
	if len(reply.Data) > 32<<10 || json.Unmarshal(reply.Data, &outcome, json.RejectUnknownMembers(true)) != nil {
		return empty, trackerUnknownOutcome(nil)
	}
	state.mu.Lock()
	prepared, dispatched := state.prepared, state.dispatched
	state.mu.Unlock()
	if outcome.Proof == nil {
		if prepared != nil || dispatched != nil || outcome.Observation.Status != args.Target || outcome.Observation.Version.IsZero() {
			return empty, trackerUnknownOutcome(nil)
		}
		outcome.Observation.TrackerWriteUID = ""
		settled = true
		return outcome.Observation, nil
	}
	if outcome.Proof.Phase == db.TrackerWriteNotSent {
		if prepared == nil || !db.TrackerOwnerProofMatchesReservation(*prepared, *outcome.Proof) || outcome.Proof.WriteUID != metadata.identity {
			return empty, trackerUnknownOutcome(nil)
		}
		recovery, ok := r.runtime.store.(db.TrackerRemoteRecoveryStore)
		if !ok {
			return empty, trackerUnknownOutcome(nil)
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := recovery.SettleTrackerOwnerNonSend(cleanup, *outcome.Proof); err != nil {
			return empty, trackerUnknownOutcome(err)
		}
		settled = true
		return empty, tracker.ErrUnavailable
	}
	if prepared == nil || dispatched == nil || !db.TrackerOwnerProofMatchesReservation(*prepared, *outcome.Proof) || outcome.Proof.WriteUID != metadata.identity {
		return empty, trackerUnknownOutcome(nil)
	}
	receipts, ok := r.runtime.store.(db.TrackerRemoteRecoveryStore)
	if !ok {
		return empty, trackerUnknownOutcome(nil)
	}
	// A retained result may outlive caller cancellation. Keep all native
	// project/request fences; failure remains unknown instead of being retried.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := receipts.AcceptTrackerOwnerResult(finalCtx, *outcome.Proof); err != nil {
		return empty, trackerUnknownOutcome(err)
	}
	var result db.TrackerStatusResult
	if json.Unmarshal(outcome.Proof.Result, &result, json.RejectUnknownMembers(true)) != nil || result.Status != args.Target || result.Version.IsZero() {
		return empty, trackerUnknownOutcome(nil)
	}
	settled = true
	return trackerObservationFromReceipt(result, metadata.identity), nil
}
func (w *trackerRootWorkerWrite) retainUnsettled() {
	w.mu.Lock()
	defer w.mu.Unlock()
	fences, ok := w.runtime.store.(db.TrackerFenceStore)
	if !ok {
		return
	}
	previous := w.prepared
	if w.dispatched != nil {
		previous = w.dispatched
	}
	if previous == nil {
		return
	}
	next := *previous
	if w.dispatched == nil {
		next.Phase = db.TrackerWriteNotSent
		next.Result = []byte(`{"not_sent":true}`)
	} else {
		next.Phase = db.TrackerWriteUnknown
	}
	// Cleanup records only what this call can prove. Exact CAS cannot replace a
	// concurrently retained result, newer attempt, or another owner's hazard.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 10*time.Second)
	defer cancel()
	_ = fences.StoreTrackerExecutionFence(ctx, previous, next, w.runtime.epoch)
}

// Wire errors retain delivery classification only. Raw provider diagnostics,
// errors, headers, URLs and credential selectors never cross an owner boundary.
type trackerWorkerFailure struct {
	HTTPStatus             int   `json:"http_status"`
	RetryAfterMilliseconds int64 `json:"retry_after_milliseconds"`
	RateLimited            bool  `json:"rate_limited"`
	AuthFailure            bool  `json:"auth_failure"`
	Ambiguous              bool  `json:"ambiguous"`
	Blocked                bool  `json:"blocked"`
}

func workerStatusFailure(err error) *trackerWorkerFailure {
	status, ok := errors.AsType[*issuesync.StatusError](err)
	if !ok {
		return nil
	}
	code := status.HTTPStatus
	if code != 0 && (code < 100 || code > 599) {
		code = 0
	}
	retry := min(max(status.RetryAfter, 0), 24*time.Hour)
	return &trackerWorkerFailure{HTTPStatus: code, RetryAfterMilliseconds: retry.Milliseconds(), RateLimited: status.RateLimited, AuthFailure: status.AuthFailure, Ambiguous: status.Ambiguous, Blocked: status.Blocked}
}
func (f trackerWorkerFailure) valid() bool {
	return (f.HTTPStatus == 0 || (f.HTTPStatus >= 100 && f.HTTPStatus <= 599)) && f.RetryAfterMilliseconds >= 0 && f.RetryAfterMilliseconds <= (24*time.Hour).Milliseconds()
}
func trackerWorkerError(reply trackerWorkerReply) error {
	if reply.Failure != nil && reply.Failure.valid() && reply.Code != "ok" {
		f := reply.Failure
		return &issuesync.StatusError{Message: "selected tracker request failed", HTTPStatus: f.HTTPStatus, RetryAfter: time.Duration(f.RetryAfterMilliseconds) * time.Millisecond, RateLimited: f.RateLimited, AuthFailure: f.AuthFailure, Ambiguous: f.Ambiguous, Blocked: f.Blocked, Cause: func() error {
			if reply.Code == "unknown" {
				return db.ErrTrackerOutcomeUnknown
			}
			return nil
		}()}
	}
	switch reply.Code {
	case "denied":
		return tracker.ErrDenied
	case "invalid":
		return tracker.ErrInvalid
	case "unknown":
		return db.ErrTrackerOutcomeUnknown
	case "capacity":
		return tracker.ErrControlCapacity
	default:
		return tracker.ErrUnavailable
	}
}

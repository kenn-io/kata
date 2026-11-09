package daemon

import (
	"context"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

// Registration spans admission, effect and detached result retention. Operator
// settlement cannot race an admitted but not yet dispatched local invocation.
func (r *TrackerRuntime) beginControlAttempt(project, operation string) (func(), error) {
	key := project + "/" + operation
	r.controlsMu.Lock()
	defer r.controlsMu.Unlock()
	if r.controls == nil {
		r.controls = map[string]struct{}{}
	}
	if _, active := r.controls[key]; active {
		return nil, tracker.ErrControlPending
	}
	if len(r.controls) >= 256 {
		return nil, tracker.ErrControlCapacity
	}
	r.controls[key] = struct{}{}
	return func() { r.controlsMu.Lock(); delete(r.controls, key); r.controlsMu.Unlock() }, nil
}

// settleTrackerControl is owner-only local bookkeeping. It neither retries the
// original effect nor changes its write/activation evidence. The daemon has one
// owning process per database; a prior process epoch cannot keep running after
// that daemon lock transfers. External provider work can still remain unknown.
func (r *TrackerRuntime) settleTrackerControl(ctx context.Context, project, operation string, operator tracker.Principal) (tracker.ControlReceipt, error) {
	if r == nil || r.store == nil {
		return tracker.ControlReceipt{}, tracker.ErrUnavailable
	}
	if operator.AuthorityInstanceUID != r.store.InstanceUID() || operator.Subject != r.store.InstanceUID() || (operator.Kind != tracker.SubjectOwnerLocal && operator.Kind != tracker.SubjectStaticOwner) {
		return tracker.ControlReceipt{}, tracker.ErrDenied
	}
	controls, ok := r.store.(db.TrackerControlStore)
	if !ok {
		return tracker.ControlReceipt{}, tracker.ErrUnavailable
	}
	r.controlsMu.Lock()
	defer r.controlsMu.Unlock()
	if _, active := r.controls[project+"/"+operation]; active {
		return tracker.ControlReceipt{}, tracker.ErrControlPending
	}
	current, err := controls.ReadTrackerControl(ctx, project, operation)
	if err != nil {
		return tracker.ControlReceipt{}, err
	}
	if current.State != tracker.ControlPending {
		return current, nil
	}
	// Older records without process ownership cannot prove local execution has
	// stopped. Supported restore normalizes those to inactive restored receipts.
	if current.ProcessEpoch == "" {
		return tracker.ControlReceipt{}, tracker.ErrControlPending
	}
	next := current
	next.State = tracker.ControlFailed
	next.Result = &tracker.ControlResult{Code: "tracker_outcome_unknown"}
	next.Settlement = &tracker.ControlSettlement{Operator: operator, RecordedAt: time.Now().UTC(), Kind: "abandon_unresolved_control"}
	if err := controls.CompleteTrackerControl(ctx, current, next); err != nil {
		return tracker.ControlReceipt{}, err
	}
	return next, nil
}

package daemon

import (
	"context"
	"encoding/json/v2"
	"sync"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

type trackerRemoteStatusKey struct{}
type trackerRemoteStatus struct {
	admission     db.TrackerStatusAdmission
	identity      string
	originalAdmit func(context.Context) error
}
type trackerWorkerSignal struct {
	CallUID    string                    `json:"call_uid"`
	SessionUID string                    `json:"session_uid"`
	Stage      string                    `json:"stage"`
	Owner      *db.TrackerExecutionFence `json:"owner,omitempty"`
}
type trackerWorkerDecision struct {
	Reservation db.TrackerExecutionFence   `json:"reservation"`
	Authority   tracker.SignedWorkerTicket `json:"authority"`
}
type trackerRootWorkerWrite struct {
	mu            sync.Mutex
	ctx           context.Context // exact root invocation; removed with its broker call
	runtime       *TrackerRuntime
	ticket        tracker.WorkerTicket
	admission     db.TrackerStatusAdmission
	originalAdmit func(context.Context) error
	prepared      *db.TrackerExecutionFence
	dispatched    *db.TrackerExecutionFence
}

func workerArgumentsExternalID(provider string, args trackerWorkerArguments) string {
	switch provider {
	case "github":
		return args.ID
	case "notion":
		if args.ID != "" {
			return "page:" + args.ID
		}
	case "linear":
		if args.ID != "" {
			return "issue:" + args.ID
		}
	case "plane":
		if args.ID != "" {
			return "work-item:" + args.ID
		}
	case "ticktick", "twenty":
		if args.ID != "" {
			return "task:" + args.ID
		}
	case "todoist":
		if args.TodoistTarget != nil && args.TodoistTarget.ID != "" {
			return "task:" + args.TodoistTarget.ID
		}
	}
	return ""
}
func (w *trackerRootWorkerWrite) stage(transport context.Context, signal trackerWorkerSignal) (trackerWorkerDecision, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	empty := trackerWorkerDecision{}
	if signal.CallUID != w.ticket.CallUID || signal.SessionUID != w.ticket.SessionUID {
		return empty, tracker.ErrDenied
	}
	ctx, stopStage := context.WithCancel(w.ctx)
	stopTransport := context.AfterFunc(transport, stopStage)
	defer func() { stopTransport(); stopStage() }()
	var cancel context.CancelFunc
	if deadline, ok := transport.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	ctx = db.WithAdditionalTransactionFence(ctx, func(_ context.Context, tx db.Transaction) error { return db.ApplyTransactionFence(transport, tx) })
	if ctx.Err() != nil || transport.Err() != nil {
		return empty, tracker.ErrUnavailable
	}
	authority, ok := ctx.Value(trackerRunAuthorityKey{}).(trackerRunAuthority)
	if !ok || authority.principal.Kind != tracker.SubjectService || authority.root != w.runtime.store.InstanceUID() {
		return empty, tracker.ErrDenied
	}
	snapshot, err := w.runtime.current()
	if err != nil || snapshot.incarnation != w.ticket.RootEpoch {
		return empty, tracker.ErrUnavailable
	}
	if _, err := w.runtime.mappedContext(ctx, snapshot, authority.project, authority.principal); err != nil {
		return empty, err
	}
	admissions, ok := w.runtime.store.(db.TrackerStatusStore)
	if !ok {
		return empty, tracker.ErrUnavailable
	}
	switch signal.Stage {
	case "prepare":
		if signal.Owner != nil {
			return empty, tracker.ErrInvalid
		}
		if w.prepared == nil {
			if err := w.originalAdmit(ctx); err != nil {
				return empty, err
			}
			checkpoints, ok := w.runtime.store.(db.TrackerOwnerCheckpointStore)
			if !ok {
				return empty, tracker.ErrUnavailable
			}
			cp, found, err := checkpoints.LoadTrackerOwnerCheckpoint(ctx, w.ticket.RootInstanceUID, w.ticket.ProjectUID, w.ticket.Selection.OwnerInstanceUID)
			if err != nil {
				return empty, err
			}
			if found && cp.Restored {
				return empty, db.ErrTrackerOutcomeUnknown
			}
			remote := db.TrackerRemoteAttempt{UID: w.ticket.CallUID, ArgumentsDigest: w.ticket.ArgumentsDigest, OwnerSequence: 1, RootEpoch: w.runtime.epoch, OwnerEpoch: w.ticket.OwnerEpoch}
			if found {
				remote.OwnerSequence = cp.Sequence + 1
				remote.OwnerPreviousHead = cp.Head
			}
			w.admission.RemoteAttempt = &remote
			w.admission.Now = time.Now().UTC()
			var prepared db.TrackerExecutionFence
			if err := w.runtime.withSnapshotAdmission(ctx, func() error {
				var err error
				prepared, err = admissions.AdmitTrackerStatusWrite(ctx, w.admission)
				return err
			}); err != nil {
				return empty, err
			}
			w.prepared = &prepared
		}
		if w.dispatched != nil {
			return empty, db.ErrTrackerOutcomeUnknown
		}
		digest, err := workerReservationDigest(*w.prepared)
		if err != nil {
			return empty, err
		}
		return w.signDecision(ctx, tracker.WorkerReserve, digest)
	case "permit":
		if signal.Owner == nil || w.prepared == nil || w.dispatched != nil {
			return empty, db.ErrTrackerOutcomeUnknown
		}
		if !db.TrackerOwnerProofMatchesReservation(*w.prepared, *signal.Owner) || signal.Owner.Phase != db.TrackerWritePrepared || signal.Owner.ActivationRequired {
			return empty, db.ErrTrackerFenceChanged
		}
		digest, err := workerPermitDigest(*w.prepared, *signal.Owner)
		if err != nil {
			return empty, err
		}
		if err := w.originalAdmit(ctx); err != nil {
			return empty, err
		}
		w.admission.ExpectedReservation = w.prepared
		w.admission.Now = time.Now().UTC()
		var dispatched db.TrackerExecutionFence
		if err := w.runtime.withSnapshotAdmission(ctx, func() error {
			var err error
			dispatched, err = admissions.AdmitTrackerStatusWrite(ctx, w.admission)
			return err
		}); err != nil {
			return empty, err
		}
		w.dispatched = &dispatched
		return w.signDecision(ctx, tracker.WorkerPermit, digest)
	}
	return empty, tracker.ErrInvalid
}
func (w *trackerRootWorkerWrite) signDecision(ctx context.Context, purpose tracker.WorkerPurpose, digest string) (trackerWorkerDecision, error) {
	if w.runtime.signing == nil {
		return trackerWorkerDecision{}, tracker.ErrUnavailable
	}
	pin, err := w.runtime.store.RootAuthority(ctx, w.ticket.ProjectUID)
	if err != nil {
		return trackerWorkerDecision{}, err
	}
	signer := w.runtime.signing.Signer()
	if !db.RootSignerMatches(pin, signer) {
		return trackerWorkerDecision{}, tracker.ErrUnavailable
	}
	ticket := w.ticket
	ticket.KeyID = pin.KeyID
	ticket.Purpose = purpose
	ticket.WriteUID = w.prepared.WriteUID
	ticket.ReservationDigest = digest
	lifetime := 30 * time.Second
	if purpose == tracker.WorkerPermit {
		lifetime = 5 * time.Second
	}
	ticket.ExpiresAt = time.Now().Add(lifetime).Unix()
	signed, err := tracker.SignWorkerTicket(ticket, signer.PrivateKey)
	if err != nil {
		return trackerWorkerDecision{}, err
	}
	return trackerWorkerDecision{Reservation: *w.prepared, Authority: signed}, nil
}
func (b *trackerWorkerBroker) writeSignal(ctx context.Context, project, owner, epoch string, signal trackerWorkerSignal) (trackerWorkerDecision, error) {
	b.mu.Lock()
	b.pruneLocked()
	p, found := b.calls[signal.CallUID]
	if !found || p.write == nil {
		b.mu.Unlock()
		return trackerWorkerDecision{}, tracker.ErrUnavailable
	}
	t := p.command.Ticket.Ticket
	if t.ProjectUID != project || t.Selection.OwnerInstanceUID != owner || t.OwnerEpoch != epoch || t.SessionUID != signal.SessionUID {
		b.mu.Unlock()
		return trackerWorkerDecision{}, tracker.ErrDenied
	}
	write := p.write
	b.mu.Unlock()
	return write.stage(ctx, signal)
}

// canonical equality is used only for immutable envelope comparisons; stored
// fence CAS retains the native byte representation and whole proof validation.
func workerFenceEqual(a, b db.TrackerExecutionFence) bool {
	left, err := json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return false
	}
	right, err := json.Marshal(b, json.Deterministic(true))
	return err == nil && string(left) == string(right)
}

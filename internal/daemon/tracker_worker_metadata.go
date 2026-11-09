package daemon

import (
	"context"
	"encoding/json/v2"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

type trackerWorkerMetadataArgs struct {
	Reference  *db.TrackerExecutionFence  `json:"reference,omitempty"`
	Checkpoint *db.TrackerOwnerCheckpoint `json:"checkpoint,omitempty"`
}
type trackerWorkerMetadataResult struct {
	Fence        *db.TrackerExecutionFence `json:"fence,omitempty"`
	Acknowledged bool                      `json:"acknowledged"`
}

func workerRecoveryMethod(method string) bool {
	return strings.HasSuffix(method, ".head") || strings.HasSuffix(method, ".receipt") || strings.HasSuffix(method, ".retire-prepared") || strings.HasSuffix(method, ".acknowledge")
}
func (r *TrackerRuntime) ownerMetadata(ctx context.Context, w *trackerOwnerWorker, t tracker.WorkerTicket, args trackerWorkerMetadataArgs) (trackerWorkerMetadataResult, error) {
	empty := trackerWorkerMetadataResult{}
	if strings.HasSuffix(t.Method, ".head") {
		if args.Reference != nil || args.Checkpoint != nil {
			return empty, tracker.ErrInvalid
		}
		fences, ok := r.store.(db.TrackerFenceStore)
		if !ok {
			return empty, tracker.ErrUnavailable
		}
		head, found, err := fences.LoadTrackerExecutionFence(ctx, t.RootInstanceUID, t.ProjectUID, "executor")
		if err != nil {
			return empty, err
		}
		if !found {
			return empty, nil
		}
		return trackerWorkerMetadataResult{Fence: &head}, nil
	}
	if strings.HasSuffix(t.Method, ".acknowledge") {
		if args.Reference != nil || args.Checkpoint == nil || args.Checkpoint.Validate() != nil || args.Checkpoint.RootInstanceUID != t.RootInstanceUID || args.Checkpoint.ProjectUID != t.ProjectUID || args.Checkpoint.Selection != t.Selection || t.WriteUID != args.Checkpoint.WriteUID || t.Purpose != tracker.WorkerAcknowledge {
			return empty, tracker.ErrInvalid
		}
		executor, ok := r.store.(db.TrackerExecutorStore)
		if !ok {
			return empty, tracker.ErrUnavailable
		}
		if err := executor.AcknowledgeTrackerExecutor(ctx, *args.Checkpoint); err != nil {
			return empty, err
		}
		return trackerWorkerMetadataResult{Acknowledged: true}, nil
	}
	reference := args.Reference
	if args.Checkpoint != nil || reference == nil || reference.Validate() != nil || reference.Role != "coordinator" || reference.Remote == nil || reference.RootInstanceUID != t.RootInstanceUID || reference.ProjectUID != t.ProjectUID || reference.Connection != t.Selection {
		return empty, tracker.ErrInvalid
	}
	fences, ok := r.store.(db.TrackerFenceStore)
	if !ok {
		return empty, tracker.ErrUnavailable
	}
	head, found, err := fences.LoadTrackerExecutionFence(ctx, t.RootInstanceUID, t.ProjectUID, "executor")
	if err != nil {
		return empty, err
	}
	if !found || !db.TrackerOwnerProofMatchesReservation(*reference, head) {
		history, ok := r.store.(db.TrackerFenceHistoryStore)
		if !ok {
			return empty, tracker.ErrUnavailable
		}
		head, found, err = history.LoadTrackerExecutionHistory(ctx, t.RootInstanceUID, t.ProjectUID, "executor", reference.WriteUID)
		if err != nil {
			return empty, err
		}
	}
	if !found || !db.TrackerOwnerProofMatchesReservation(*reference, head) {
		return empty, db.ErrNotFound
	}
	if strings.HasSuffix(t.Method, ".retire-prepared") {
		if head.Phase == db.TrackerWriteNotSent {
			return trackerWorkerMetadataResult{Fence: &head}, nil
		}
		if head.Phase != db.TrackerWritePrepared || head.ActivationRequired {
			return empty, db.ErrTrackerOutcomeUnknown
		}
		// Hold the attempt registry until the retirement CAS commits. The provider
		// invocation keeps its entry through native retention/finalization. A fresh
		// daemon also holds Kata's single-process lock; restored records cannot pass.
		w.mu.Lock()
		defer w.mu.Unlock()
		if call, live := w.calls[head.Remote.UID]; live && !call.complete {
			return empty, db.ErrTrackerOutcomeUnknown
		}
		recovery, ok := r.store.(db.TrackerRemoteRecoveryStore)
		if !ok {
			return empty, tracker.ErrUnavailable
		}
		head, err = recovery.RetireTrackerExecutorPreparation(ctx, head, r.epoch)
		if err != nil {
			return empty, err
		}
	}
	return trackerWorkerMetadataResult{Fence: &head}, nil
}
func (r *TrackerRuntime) metadataRemoteRun(ctx context.Context, project, provider string, selection tracker.Selection) (*trackerRemoteRun, error) {
	if selection.Validate() != nil || !tracker.SupportedProvider(provider) || selection.OwnerInstanceUID == r.store.InstanceUID() {
		return nil, tracker.ErrInvalid
	}
	for _, owner := range r.workerBroker.owners(project, time.Now()) {
		if owner.OwnerInstanceUID != selection.OwnerInstanceUID {
			continue
		}
		runUID, err := uid.New()
		if err != nil {
			return nil, err
		}
		sessionUID, err := uid.New()
		if err != nil {
			return nil, err
		}
		return &trackerRemoteRun{metadata: true, runtime: r, project: project, principal: tracker.Principal{AuthorityInstanceUID: r.store.InstanceUID(), Kind: tracker.SubjectService, Subject: "tracker-recovery"}, descriptor: tracker.PublicDescriptor{Selection: selection, Provider: provider, Label: "Retained tracker connection"}, ownerEpoch: owner.OwnerEpoch, resource: "tracker-metadata:" + project, runUID: runUID, sessionUID: sessionUID}, nil
	}
	return nil, tracker.ErrUnavailable
}
func (r *TrackerRuntime) sendRemoteAcknowledgement(ctx context.Context, project, provider, owner string) error {
	checkpoints, ok := r.store.(db.TrackerOwnerCheckpointStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	cp, found, err := checkpoints.LoadTrackerOwnerCheckpoint(ctx, r.store.InstanceUID(), project, owner)
	if err != nil || !found {
		return err
	}
	run, err := r.metadataRemoteRun(ctx, project, provider, cp.Selection)
	if err != nil {
		return err
	}
	var result trackerWorkerMetadataResult
	if err := run.invoke(ctx, "acknowledge", tracker.ActionRecover, trackerWorkerMetadataArgs{Checkpoint: &cp}, &result, true); err != nil {
		return err
	}
	if !result.Acknowledged {
		return tracker.ErrUnavailable
	}
	return nil
}
func (r *TrackerRuntime) recoverRemoteFence(ctx context.Context, reference db.TrackerExecutionFence) error {
	provider, _, ok := strings.Cut(reference.SourceKey, ":")
	if !ok || !tracker.SupportedProvider(provider) {
		return db.ErrTrackerFenceInvalid
	}
	run, err := r.metadataRemoteRun(ctx, reference.ProjectUID, provider, reference.Connection)
	if err != nil {
		return err
	}
	var result trackerWorkerMetadataResult
	args := trackerWorkerMetadataArgs{Reference: &reference}
	if err := run.invoke(ctx, "receipt", tracker.ActionRecover, args, &result, true); err != nil {
		return err
	}
	if result.Fence == nil || !db.TrackerOwnerProofMatchesReservation(reference, *result.Fence) {
		return db.ErrTrackerFenceChanged
	}
	if result.Fence.Phase == db.TrackerWritePrepared {
		if err := run.invoke(ctx, "retire-prepared", tracker.ActionRecover, args, &result, true); err != nil {
			return err
		}
	}
	if result.Fence == nil || !db.TrackerOwnerProofMatchesReservation(reference, *result.Fence) {
		return db.ErrTrackerFenceChanged
	}
	recovery, ok := r.store.(db.TrackerRemoteRecoveryStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	switch result.Fence.Phase {
	case db.TrackerWriteNotSent:
		return recovery.SettleTrackerOwnerNonSend(ctx, *result.Fence)
	case db.TrackerWriteConfirmed, db.TrackerWriteAcknowledged:
		return recovery.AcceptTrackerOwnerResult(ctx, *result.Fence)
	default:
		return db.ErrTrackerOutcomeUnknown
	}
}

// acceptWorkerTerminal authenticates the exact reverse owner from the live
// relay path in its handler. It exercises no provider grant/credential and
// never acknowledges a local event or clears restoration barriers.
func acceptWorkerTerminal(ctx context.Context, cfg ServerConfig, project, owner string, signal trackerWorkerSignal) error {
	proof := signal.Owner
	if signal.Stage != "terminal" || proof == nil || proof.Validate() != nil || proof.Role != "executor" || proof.RootInstanceUID != cfg.DB.InstanceUID() || proof.ProjectUID != project || proof.Connection.OwnerInstanceUID != owner || proof.Remote == nil || proof.Remote.UID != signal.CallUID {
		return tracker.ErrInvalid
	}
	recovery, ok := cfg.DB.(db.TrackerRemoteRecoveryStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	switch proof.Phase {
	case db.TrackerWriteNotSent:
		return recovery.SettleTrackerOwnerNonSend(ctx, *proof)
	case db.TrackerWriteConfirmed, db.TrackerWriteAcknowledged:
		return recovery.AcceptTrackerOwnerResult(ctx, *proof)
	default:
		return tracker.ErrInvalid
	}
}
func decodeWorkerMetadata(raw []byte) (trackerWorkerMetadataArgs, error) {
	var args trackerWorkerMetadataArgs
	if json.Unmarshal(raw, &args, json.RejectUnknownMembers(true)) != nil {
		return args, tracker.ErrInvalid
	}
	return args, nil
}

func (r *TrackerRuntime) acknowledgeRemoteCheckpoint(ctx context.Context, project, provider, owner string) error {
	checkpoints, ok := r.store.(db.TrackerOwnerCheckpointStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	cp, found, err := checkpoints.LoadTrackerOwnerCheckpoint(ctx, r.store.InstanceUID(), project, owner)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := r.recoverRemoteOwner(ctx, project, provider, cp.Selection); err != nil {
		return err
	}
	return r.sendRemoteAcknowledgement(ctx, project, provider, owner)
}
func (r *TrackerRuntime) recoverRemoteOwner(ctx context.Context, project, provider string, selection tracker.Selection) error {
	run, err := r.metadataRemoteRun(ctx, project, provider, selection)
	if err != nil {
		return err
	}
	var result trackerWorkerMetadataResult
	if err := run.invoke(ctx, "head", tracker.ActionRecover, trackerWorkerMetadataArgs{}, &result, true); err != nil {
		return err
	}
	checkpoints, ok := r.store.(db.TrackerOwnerCheckpointStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	cp, cpFound, err := checkpoints.LoadTrackerOwnerCheckpoint(ctx, r.store.InstanceUID(), project, selection.OwnerInstanceUID)
	if err != nil {
		return err
	}
	if result.Fence == nil {
		if cpFound {
			return db.ErrTrackerOutcomeUnknown
		}
		return nil
	}
	owner := *result.Fence
	if owner.Validate() != nil || owner.Role != "executor" || owner.RootInstanceUID != r.store.InstanceUID() || owner.ProjectUID != project || owner.Connection.OwnerInstanceUID != selection.OwnerInstanceUID || owner.Remote == nil {
		return db.ErrTrackerFenceInvalid
	}
	if cpFound && cp.Head == db.TrackerFenceHead(owner) && cp.Attempt == *owner.Remote && cp.Selection == owner.Connection && cp.WriteUID == owner.WriteUID && cp.ResultDigest == tracker.WorkerArgumentsDigest(owner.Result) {
		return r.sendRemoteAcknowledgement(ctx, project, provider, selection.OwnerInstanceUID)
	}
	fences, ok := r.store.(db.TrackerFenceStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	reference, found, err := fences.LoadTrackerExecutionFence(ctx, r.store.InstanceUID(), project, "coordinator")
	if err != nil {
		return err
	}
	if !found || !db.TrackerOwnerProofMatchesReservation(reference, owner) {
		history, ok := r.store.(db.TrackerFenceHistoryStore)
		if !ok {
			return tracker.ErrUnavailable
		}
		reference, found, err = history.LoadTrackerExecutionHistory(ctx, r.store.InstanceUID(), project, "coordinator", owner.WriteUID)
		if err != nil {
			return err
		}
	}
	if !found || !db.TrackerOwnerProofMatchesReservation(reference, owner) {
		return db.ErrTrackerFenceChanged
	}
	if err := r.recoverRemoteFence(ctx, reference); err != nil {
		return err
	}
	if receipts, ok := r.store.(db.TrackerRecoveryReceiptStore); ok {
		if err := receipts.AcknowledgeTrackerRecovery(ctx, project); err != nil {
			return err
		}
	} else {
		return tracker.ErrUnavailable
	}
	return r.sendRemoteAcknowledgement(ctx, project, provider, selection.OwnerInstanceUID)
}

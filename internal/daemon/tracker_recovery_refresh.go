package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

func registerTrackerRecoveryRefresh(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "refreshTrackerRecovery", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/tracker-recovery/refresh"}, func(ctx context.Context, in *api.TrackerRecoveryRefreshRequest) (*api.TrackerRecoveryRefreshResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		ctx, err := trackerClientContext(ctx, in.Client)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		project, err := trackerOperatorContext(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		if cfg.TrackerRuntime == nil {
			return nil, trackerAPIError(tracker.ErrUnavailable)
		}
		if err := cfg.TrackerRuntime.refreshTrackerRecovery(ctx, project); err != nil {
			return nil, trackerAPIError(err)
		}
		return &api.TrackerRecoveryRefreshResponse{Body: api.TrackerRecoveryRefreshOut{RootInstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, ReceiptsRefreshed: true}}, nil
	})
}
func (r *TrackerRuntime) refreshTrackerRecovery(ctx context.Context, project db.Project) error {
	binding, err := r.store.FederationBindingByProject(ctx, project.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return err
	}
	if err == nil && binding.Role == db.FederationRoleSpoke {
		return db.ErrIssueSyncFederationBinding
	}
	fences, ok := r.store.(db.TrackerFenceStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	head, found, err := fences.LoadTrackerExecutionFence(ctx, r.store.InstanceUID(), project.UID, "coordinator")
	if err != nil {
		return err
	}
	type owner struct {
		selection tracker.Selection
		provider  string
	}
	owners := map[string]owner{}
	if found && head.Remote != nil {
		if head.Phase == db.TrackerWritePrepared {
			if r.workerBroker.liveWrite(head.Remote.UID) {
				return tracker.ErrControlPending
			}
			retire, ok := r.store.(db.TrackerCoordinatorRetirementStore)
			if !ok {
				return tracker.ErrUnavailable
			}
			head, err = retire.RetireTrackerCoordinatorPreparation(ctx, head, r.epoch)
			if err != nil {
				return err
			}
		}
		if head.Connection.OwnerInstanceUID != r.store.InstanceUID() {
			provider, _, ok := strings.Cut(head.SourceKey, ":")
			if !ok || !tracker.SupportedProvider(provider) {
				return db.ErrTrackerFenceInvalid
			}
			owners[head.Connection.OwnerInstanceUID] = owner{head.Connection, provider}
		}
	}
	current, err := r.store.IssueSyncBindingByProject(ctx, project.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return err
	}
	if err == nil {
		selection, err := db.IssueSyncConnectionFromConfig(current.Config)
		if err != nil {
			return err
		}
		if selection != nil && selection.OwnerInstanceUID != r.store.InstanceUID() {
			owners[selection.OwnerInstanceUID] = owner{*selection, current.Provider}
		}
	}
	// Current binding/global head need not mention a previously acknowledged
	// owner whose final acknowledgement was lost before rebind to another hub.
	recoveryErr := r.refreshRetainedTrackerOwners(ctx, project.UID, func(ctx context.Context, selection tracker.Selection, provider string) error {
		delete(owners, selection.OwnerInstanceUID)
		return r.recoverRemoteOwner(ctx, project.UID, provider, selection)
	})
	for _, owner := range owners {
		recoveryErr = errors.Join(recoveryErr, r.recoverRemoteOwner(ctx, project.UID, owner.provider, owner.selection))
	}
	receipts, ok := r.store.(db.TrackerRecoveryReceiptStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	if err := receipts.AcknowledgeTrackerRecovery(ctx, project.UID); err != nil && !errors.Is(err, db.ErrNotFound) {
		return err
	}
	head, found, err = fences.LoadTrackerExecutionFence(ctx, r.store.InstanceUID(), project.UID, "coordinator")
	if err != nil {
		return err
	}
	if found && head.Phase != db.TrackerWriteAcknowledged && head.Phase != db.TrackerWriteNotSent {
		return errors.Join(recoveryErr, db.ErrTrackerOutcomeUnknown)
	}
	return recoveryErr
}
func (b *trackerWorkerBroker) liveWrite(call string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, found := b.calls[call]
	return found && p.write != nil && !p.completed && p.ctx.Err() == nil
}

func (r *TrackerRuntime) refreshRetainedTrackerOwners(ctx context.Context, project string, visit func(context.Context, tracker.Selection, string) error) error {
	lister, ok := r.store.(db.TrackerOwnerCheckpointLister)
	if !ok {
		return tracker.ErrUnavailable
	}
	history, ok := r.store.(db.TrackerFenceHistoryStore)
	if !ok {
		return tracker.ErrUnavailable
	}
	seen := map[string]bool{}
	after := ""
	var recoveryErr error
	for {
		page, err := lister.ListTrackerOwnerCheckpoints(ctx, r.store.InstanceUID(), project, after, 128)
		if err != nil {
			return err
		}
		for _, cp := range page {
			if cp.OwnerInstanceUID == r.store.InstanceUID() {
				continue
			}
			proof, found, err := history.LoadTrackerExecutionHistory(ctx, r.store.InstanceUID(), project, "coordinator", cp.WriteUID)
			if err != nil {
				return err
			}
			if !found || proof.Connection != cp.Selection || proof.Remote == nil || *proof.Remote != cp.Attempt || proof.WriteUID != cp.WriteUID {
				return db.ErrTrackerFenceChanged
			}
			expected, err := db.TrackerExecutorPreparationForCheckpoint(proof)
			if err != nil || db.TrackerFenceHead(expected) != cp.Head || tracker.WorkerArgumentsDigest(proof.Result) != cp.ResultDigest {
				return db.ErrTrackerFenceChanged
			}
			provider, _, valid := strings.Cut(proof.SourceKey, ":")
			if !valid || !tracker.SupportedProvider(provider) {
				return db.ErrTrackerFenceInvalid
			}
			seen[cp.OwnerInstanceUID] = true
			recoveryErr = errors.Join(recoveryErr, visit(ctx, cp.Selection, provider))
			if ctx.Err() != nil {
				return errors.Join(recoveryErr, ctx.Err())
			}
		}
		if len(page) < 128 {
			break
		}
		after = page[len(page)-1].OwnerInstanceUID
	}
	// Root-only retirement intentionally creates no settled owner checkpoint.
	// Retained semantic history is the discoverable obligation for that first
	// prepared owner, even after another owner replaces the global head/binding.
	retired, ok := r.store.(db.TrackerRecoveryHistoryLister)
	if !ok {
		return errors.Join(recoveryErr, tracker.ErrUnavailable)
	}
	after = ""
	for {
		page, err := retired.ListTrackerRecoveryHistory(ctx, r.store.InstanceUID(), project, after, 128)
		if err != nil {
			return errors.Join(recoveryErr, err)
		}
		for _, proof := range page {
			owner := proof.Connection.OwnerInstanceUID
			if proof.Remote == nil || owner == r.store.InstanceUID() || seen[owner] {
				continue
			}
			provider, _, valid := strings.Cut(proof.SourceKey, ":")
			if !valid || !tracker.SupportedProvider(provider) {
				return errors.Join(recoveryErr, db.ErrTrackerFenceInvalid)
			}
			seen[owner] = true
			recoveryErr = errors.Join(recoveryErr, visit(ctx, proof.Connection, provider))
			if ctx.Err() != nil {
				return errors.Join(recoveryErr, ctx.Err())
			}
		}
		if len(page) < 128 {
			return recoveryErr
		}
		after = page[len(page)-1].WriteUID
	}
}

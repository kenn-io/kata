package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

func trackerOperatorContext(ctx context.Context, cfg ServerConfig, projectID int64) (db.Project, error) {
	if cfg.HostAccess != nil || !projectOwnerAuthority(ctx) {
		return db.Project{}, tracker.ErrDenied
	}
	p, err := trackerRequestPrincipal(ctx, cfg.DB.InstanceUID())
	if err != nil {
		return db.Project{}, err
	}
	if p.Kind != tracker.SubjectOwnerLocal && p.Kind != tracker.SubjectStaticOwner {
		return db.Project{}, tracker.ErrDenied
	}
	return activeProjectByID(ctx, cfg.DB, projectID)
}
func registerTrackerInspectionHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "settleTrackerControl", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/tracker-operations/{uid}/settle"}, func(ctx context.Context, in *api.TrackerControlSettlementRequest) (*api.TrackerOperationResponse, error) {
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
		operator, err := trackerRequestPrincipal(ctx, cfg.DB.InstanceUID())
		if err != nil {
			return nil, trackerAPIError(err)
		}
		receipt, err := cfg.TrackerRuntime.settleTrackerControl(ctx, project.UID, in.UID, operator)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		return &api.TrackerOperationResponse{Body: trackerOperationOut(receipt)}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "inspectTrackerRecovery", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/tracker-recovery"}, func(ctx context.Context, in *api.TrackerProjectRequest) (*api.TrackerRecoveryResponse, error) {
		ctx, err := trackerClientContext(ctx, in.Client)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		project, err := trackerOperatorContext(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		result := api.TrackerRecoveryOut{RootInstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, Role: "coordinator", WritesBlocked: true, ActivationRequired: true}
		binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, trackerAPIError(err)
		}
		if err == nil && binding.Role == db.FederationRoleSpoke {
			pin, err := cfg.DB.RootAuthority(ctx, project.UID)
			if err != nil {
				return nil, trackerAPIError(err)
			}
			result.RootInstanceUID = pin.AuthorityUID
			result.Role = "executor"
		}
		fences, ok := cfg.DB.(db.TrackerFenceStore)
		if !ok {
			return nil, trackerAPIError(tracker.ErrUnavailable)
		}
		head, found, err := fences.LoadTrackerExecutionFence(ctx, result.RootInstanceUID, project.UID, result.Role)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		settled := !found
		if found {
			result.Selection = &head.Connection
			result.WriteUID = head.WriteUID
			result.PendingEventUID = head.PendingEventUID
			result.Phase = string(head.Phase)
			result.ProviderAcknowledged = head.ProviderAcknowledged
			settled = head.Phase == db.TrackerWriteAcknowledged || head.Phase == db.TrackerWriteNotSent
		}
		if result.Role == "coordinator" && cfg.TrackerRuntime != nil {
			activation, ok := cfg.DB.(db.TrackerActivationStore)
			if ok {
				a, present, err := activation.LoadTrackerActivation(ctx, project.UID)
				if err != nil {
					return nil, trackerAPIError(err)
				}
				_, policyErr := cfg.TrackerRuntime.current()
				result.ActivationRequired = !present || !a.Active || a.ProcessEpoch != cfg.TrackerRuntime.epoch || policyErr != nil || (found && head.ActivationRequired)
				result.WritesBlocked = true
				current, readErr := cfg.DB.IssueSyncBindingByProject(ctx, project.ID)
				if readErr != nil && !errors.Is(readErr, db.ErrNotFound) {
					return nil, trackerAPIError(readErr)
				}
				if readErr == nil && current.Enabled && a.BindingID == current.ID && a.SourceKey == current.SourceKey {
					selection, selectionErr := db.IssueSyncConnectionFromConfig(current.Config)
					mode, modeErr := db.IssueStatusMode(current.Config)
					if selectionErr == nil && selection != nil && a.Connection == *selection && modeErr == nil && mode == "two-way" {
						routed, routeErr := cfg.TrackerRuntime.RouteBinding(ctx, current)
						if routeErr == nil {
							_, statusErr := cfg.TrackerRuntime.selectedConnection(routed, current.Provider, tracker.ActionStatus)
							result.WritesBlocked = !settled || result.ActivationRequired || statusErr != nil
						}
					}
				}
			}
		}
		restore, ok := cfg.DB.(db.TrackerRestoreStateStore)
		if !ok {
			return nil, trackerAPIError(tracker.ErrUnavailable)
		}
		result.RestoredHistoryGap, err = restore.TrackerRestoreHistoryBlocked(ctx, project.UID)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		if result.RestoredHistoryGap {
			result.WritesBlocked = true
			result.ActivationRequired = true
		}
		return &api.TrackerRecoveryResponse{Body: result}, nil
	})
}

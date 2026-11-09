package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

var errTrackerRelayUnavailable = errors.New("tracker control capability is unavailable on this relay path")

func registerTrackerHandlers(humaAPI huma.API, cfg ServerConfig) {
	registerTrackerRelayHandlers(humaAPI, cfg)
	registerTrackerInspectionHandlers(humaAPI, cfg)
	huma.Register(humaAPI, huma.Operation{OperationID: "executeTrackerOperation", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/tracker-operations"}, func(ctx context.Context, in *api.TrackerOperationRequest) (*api.TrackerOperationResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		ctx, err := trackerClientContext(ctx, in.Client)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		ctx, project, principal, err := trackerMemberContext(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		out, err := trackerProjectRequest(ctx, cfg, project, principal, api.TrackerRelayPayload{Kind: "execute", UID: in.Body.UID, Operation: &in.Body})
		if err != nil {
			return nil, trackerAPIError(err)
		}
		return &api.TrackerOperationResponse{Body: *out.Operation}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "getTrackerOperation", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/tracker-operations/{uid}"}, func(ctx context.Context, in *api.TrackerOperationReadRequest) (*api.TrackerOperationResponse, error) {
		ctx, err := trackerClientContext(ctx, in.Client)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		ctx, project, principal, err := trackerMemberContext(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		out, err := trackerProjectRequest(ctx, cfg, project, principal, api.TrackerRelayPayload{Kind: "receipt", UID: in.UID})
		if err != nil {
			return nil, trackerAPIError(err)
		}
		return &api.TrackerOperationResponse{Body: *out.Operation}, nil
	})

	huma.Register(humaAPI, huma.Operation{OperationID: "listTrackerConnections", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/tracker-connections"}, func(ctx context.Context, in *api.TrackerProjectRequest) (*api.TrackerConnectionsResponse, error) {
		ctx, err := trackerClientContext(ctx, in.Client)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		ctx, project, principal, err := trackerMemberContext(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, trackerAPIError(err)
		}
		operation, err := uid.New()
		if err != nil {
			return nil, trackerAPIError(err)
		}
		out, err := trackerProjectRequest(ctx, cfg, project, principal, api.TrackerRelayPayload{Kind: "discover", UID: operation})
		if err != nil {
			return nil, trackerAPIError(err)
		}
		return &api.TrackerConnectionsResponse{Body: *out.Connections}, nil
	})
}

func trackerMemberContext(ctx context.Context, cfg ServerConfig, projectID int64) (context.Context, db.Project, tracker.Principal, error) {
	if cfg.HostAccess != nil {
		return ctx, db.Project{}, tracker.Principal{}, errTrackerPrincipalUnsupported
	}
	principal, err := trackerRequestPrincipal(ctx, cfg.DB.InstanceUID())
	if err != nil {
		return ctx, db.Project{}, tracker.Principal{}, err
	}
	if err := revalidateIssueScopedPrincipal(ctx, cfg.DB); err != nil {
		return ctx, db.Project{}, tracker.Principal{}, err
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return ctx, db.Project{}, tracker.Principal{}, err
	}
	if cfg.TrackerRuntime == nil {
		return ctx, db.Project{}, tracker.Principal{}, tracker.ErrUnavailable
	}
	if request, ok := PrincipalFromContext(ctx); ok && request.Kind == PrincipalDBToken {
		ctx = db.WithAdditionalTransactionFence(ctx, db.ActorTokenTransactionFence(db.APIToken{ID: request.TokenID, Actor: request.Actor, Scope: request.Scope, ExpiresAt: request.ExpiresAt}))
	}
	return ctx, project, principal, nil
}

func trackerAPIError(err error) error {
	if errors.Is(err, db.ErrTrackerOutcomeUnknown) {
		return api.NewError(http.StatusConflict, "tracker_outcome_unknown", db.ErrTrackerOutcomeUnknown.Error(), "", nil)
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	status, code, message := http.StatusServiceUnavailable, "tracker_unavailable", "selected tracker connection is unavailable"
	switch {
	case errors.Is(err, errTrackerPrincipalUnsupported):
		status, code, message = http.StatusForbidden, "tracker_principal_unsupported", errTrackerPrincipalUnsupported.Error()
	case errors.Is(err, tracker.ErrDenied):
		status, code, message = http.StatusForbidden, "tracker_denied", tracker.ErrDenied.Error()
	case errors.Is(err, db.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "resource not found"
	case errors.Is(err, errTrackerRelayUnavailable):
		code, message = "tracker_capability_unavailable", errTrackerRelayUnavailable.Error()
	case errors.Is(err, tracker.ErrAmbiguous):
		status, code, message = http.StatusConflict, "tracker_ambiguous", tracker.ErrAmbiguous.Error()
	case errors.Is(err, tracker.ErrSelectionRequired):
		status, code, message = http.StatusBadRequest, "tracker_selection_required", tracker.ErrSelectionRequired.Error()
	case errors.Is(err, tracker.ErrControlConflict):
		status, code, message = http.StatusConflict, "tracker_operation_conflict", tracker.ErrControlConflict.Error()
	case errors.Is(err, tracker.ErrControlPending):
		status, code, message = http.StatusConflict, "tracker_operation_pending", tracker.ErrControlPending.Error()
	case errors.Is(err, tracker.ErrControlCapacity):
		status, code, message = http.StatusTooManyRequests, "tracker_capacity", tracker.ErrControlCapacity.Error()
	case errors.Is(err, db.ErrTrackerActivationRequired):
		status, code, message = http.StatusConflict, "tracker_activation_required", db.ErrTrackerActivationRequired.Error()
	case errors.Is(err, db.ErrIssueSyncBindingChanged):
		status, code, message = http.StatusConflict, "tracker_binding_changed", db.ErrIssueSyncBindingChanged.Error()
	case errors.Is(err, errTrackerOperationUnsupported):
		status, code, message = http.StatusUnprocessableEntity, "tracker_operation_unsupported", errTrackerOperationUnsupported.Error()
	case errors.Is(err, db.ErrIssueSyncNotEnabled):
		status, code, message = http.StatusConflict, "tracker_binding_disabled", "tracker binding is disabled"
	case errors.Is(err, db.ErrIssueSyncAlreadyRunning):
		status, code, message = http.StatusConflict, "tracker_running", "tracker binding is already running"
	case errors.Is(err, db.ErrIssueSyncProjectAlreadyBound):
		status, code, message = http.StatusConflict, "tracker_project_already_bound", "project already has a different tracker binding"
	case errors.Is(err, tracker.ErrInvalid):
		status, code, message = http.StatusBadRequest, "validation", tracker.ErrInvalid.Error()
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		status, code, message = http.StatusGatewayTimeout, "tracker_timeout", "tracker request timed out"
	}
	return api.NewError(status, code, message, "", nil)
}

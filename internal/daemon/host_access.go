package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
)

// ErrHostAccessDenied is the private adapter sentinel for a host decision that
// must not disclose resource existence.
var ErrHostAccessDenied = errors.New("host access denied")

// HostOperation is the internal representation passed through the public
// service adapter.
type HostOperation struct {
	ID          string
	Method      string
	Path        string
	PathParams  map[string]string
	ProjectIDs  []int64
	ProjectUIDs []string
	AllProjects bool
}

// HostAccessRequest contains only generic request identity and route facts.
type HostAccessRequest struct {
	Subject   string
	Actor     string
	Operation HostOperation
}

// HostAccessDecision contains optional long-lived authorization state.
type HostAccessDecision struct {
	Revalidate func(context.Context) error
}

// HostAccessController is implemented by the public package adapter.
type HostAccessController interface {
	Authorize(context.Context, HostAccessRequest) (HostAccessDecision, error)
}

type hostAccessStateContextKey struct{}

type hostAccessState struct {
	controller HostAccessController
	request    HostAccessRequest
	decision   HostAccessDecision
	authorized bool
}

func requireHostAccessLease(ctx context.Context) error {
	if state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); ok {
		if !state.authorized || state.decision.Revalidate == nil {
			return api.NewError(http.StatusServiceUnavailable, "access_lease_required",
				"long-lived access lease required", "", nil)
		}
		if err := state.decision.Revalidate(ctx); err != nil {
			return api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
		return nil
	}
	return nil
}

func revalidateHostAccess(ctx context.Context) error {
	if state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); ok {
		if !state.authorized || state.decision.Revalidate == nil {
			return nil
		}
		return state.decision.Revalidate(ctx)
	}
	return nil
}

func withHostAccess(humaAPI huma.API, controller HostAccessController) {
	if controller == nil {
		return
	}
	humaAPI.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		operation := ctx.Operation()
		if operation == nil || strings.TrimSpace(operation.OperationID) == "" {
			writeHostAccessError(ctx, http.StatusServiceUnavailable,
				"access_unavailable", "access decision unavailable")
			return
		}
		principal, ok := PrincipalFromContext(ctx.Context())
		if ok && !validHostPrincipal(principal) {
			writeHostAccessError(ctx, http.StatusUnauthorized,
				"authentication_required", "authentication required")
			return
		}
		if !ok {
			if hostSelfAuthenticatedOperation(operation.OperationID) &&
				hasBearerHeader(ctx.Header(authHeader)) {
				next(ctx)
				return
			}
			writeHostAccessError(ctx, http.StatusUnauthorized,
				"authentication_required", "authentication required")
			return
		}

		pathParams := make(map[string]string)
		for _, parameter := range operation.Parameters {
			if parameter != nil && parameter.In == "path" {
				pathParams[parameter.Name] = ctx.Param(parameter.Name)
			}
		}
		request := HostAccessRequest{
			Subject: principal.Subject,
			Actor:   principal.Actor,
			Operation: HostOperation{
				ID: operation.OperationID, Method: operation.Method, Path: operation.Path,
				PathParams: pathParams,
			},
		}
		if projectID, ok := positiveProjectID(pathParams["project_id"]); ok {
			request.Operation.ProjectIDs = []int64{projectID}
		}
		if hostAccessRequiresAllProjects(operation.OperationID) {
			request.Operation.AllProjects = true
		}
		state := &hostAccessState{controller: controller, request: request}
		if hostAccessResolvedByHandler(operation.OperationID) {
			next(huma.WithValue(ctx, hostAccessStateContextKey{}, state))
			return
		}
		if len(request.Operation.ProjectIDs) == 0 && !hostAccessOperationWithoutProjectData(operation.OperationID) {
			request.Operation.AllProjects = true
			state.request.Operation.AllProjects = true
		}
		decision, err := controller.Authorize(ctx.Context(), request)
		if errors.Is(err, ErrHostAccessDenied) {
			writeHostAccessError(ctx, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if err != nil {
			writeHostAccessError(ctx, http.StatusServiceUnavailable,
				"access_unavailable", "access decision unavailable")
			return
		}
		state.decision = decision
		state.authorized = true
		next(huma.WithValue(ctx, hostAccessStateContextKey{}, state))
	})
}

func hostSelfAuthenticatedOperation(operationID string) bool {
	switch operationID {
	case "getFederationProjectMetadata",
		"pollFederationProjectEvents",
		"ingestFederationProjectEvents",
		"acquireIssueLease",
		"renewIssueLease",
		"releaseIssueLease",
		"getIssueLeaseStatus":
		return true
	default:
		return false
	}
}

func hostAccessResolvedByHandler(operationID string) bool {
	switch operationID {
	case "mergeProject",
		"moveIssue",
		"listAllIssues",
		"createFederationEnrollment":
		return true
	default:
		return false
	}
}

// hostAccessRequiresAllProjects identifies operations whose result or side
// effects can depend on relationships outside the project named in the URL.
// Requiring complete authority before dispatch keeps denials from becoming
// project-existence or relationship-state oracles.
func hostAccessRequiresAllProjects(operationID string) bool {
	switch operationID {
	case "purgeIssue", "purgeProject", "closeIssue", "readyIssues", "showIssueByUID",
		"importIssues", "pollProjectEvents", "streamEvents", "auditCloses", "digestProject",
		"deleteLink", "rewriteAuthorIdentity":
		return true
	default:
		return false
	}
}

func hostAccessOperationWithoutProjectData(operationID string) bool {
	switch operationID {
	case "ping", "health", "instance":
		return true
	default:
		return false
	}
}

func positiveProjectID(raw string) (int64, bool) {
	projectID, err := strconv.ParseInt(raw, 10, 64)
	return projectID, err == nil && projectID > 0
}

func authorizeHostProjectScope(
	ctx context.Context,
	projectIDs []int64,
	projectUIDs []string,
	allProjects bool,
) (context.Context, error) {
	state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState)
	if !ok {
		return ctx, nil
	}
	if state.authorized && state.request.Operation.AllProjects {
		return ctx, nil
	}
	previousProjectIDs := len(state.request.Operation.ProjectIDs)
	previousProjectUIDs := len(state.request.Operation.ProjectUIDs)
	previousAllProjects := state.request.Operation.AllProjects
	state.request.Operation.ProjectIDs = appendUniqueInt64(
		state.request.Operation.ProjectIDs, projectIDs...)
	state.request.Operation.ProjectUIDs = appendUniqueString(
		state.request.Operation.ProjectUIDs, projectUIDs...)
	state.request.Operation.AllProjects = state.request.Operation.AllProjects || allProjects
	scopeChanged := previousProjectIDs != len(state.request.Operation.ProjectIDs) ||
		previousProjectUIDs != len(state.request.Operation.ProjectUIDs) ||
		previousAllProjects != state.request.Operation.AllProjects
	if state.authorized && !scopeChanged {
		return ctx, nil
	}
	decision, err := state.controller.Authorize(ctx, state.request)
	if errors.Is(err, ErrHostAccessDenied) {
		return ctx, api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
	}
	if err != nil {
		return ctx, api.NewError(http.StatusServiceUnavailable,
			"access_unavailable", "access decision unavailable", "", nil)
	}
	state.decision = decision
	state.authorized = true
	return ctx, nil
}

func appendUniqueInt64(values []int64, additional ...int64) []int64 {
	for _, candidate := range additional {
		if candidate <= 0 {
			continue
		}
		found := false
		for _, value := range values {
			if value == candidate {
				found = true
				break
			}
		}
		if !found {
			values = append(values, candidate)
		}
	}
	return values
}

func appendUniqueString(values []string, additional ...string) []string {
	for _, candidate := range additional {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		found := false
		for _, value := range values {
			if value == candidate {
				found = true
				break
			}
		}
		if !found {
			values = append(values, candidate)
		}
	}
	return values
}

func authorizeHostHTTP(
	w http.ResponseWriter,
	r *http.Request,
	controller HostAccessController,
	operation HostOperation,
) bool {
	if controller == nil {
		return true
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || !validHostPrincipal(principal) {
		api.WriteEnvelope(w, http.StatusUnauthorized,
			"authentication_required", "authentication required")
		return false
	}
	_, err := controller.Authorize(r.Context(), HostAccessRequest{
		Subject: principal.Subject, Actor: principal.Actor, Operation: operation,
	})
	if errors.Is(err, ErrHostAccessDenied) {
		api.WriteEnvelope(w, http.StatusNotFound, "not_found", "resource not found")
		return false
	}
	if err != nil {
		api.WriteEnvelope(w, http.StatusServiceUnavailable,
			"access_unavailable", "access decision unavailable")
		return false
	}
	return true
}

func writeHostAccessError(ctx huma.Context, status int, code, message string) {
	body, _ := json.Marshal(api.ErrorEnvelope{
		Status: status,
		Error:  api.ErrorBody{Code: code, Message: message},
	})
	ctx.SetHeader("Content-Type", "application/json")
	ctx.SetStatus(status)
	_, _ = ctx.BodyWriter().Write(body)
}

func internalAPIError(err error) error {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
}

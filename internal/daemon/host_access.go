package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	ID         string
	Method     string
	Path       string
	PathParams map[string]string
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

type hostAccessDecisionContextKey struct{}

func requireHostAccessLease(ctx context.Context) error {
	decision, ok := ctx.Value(hostAccessDecisionContextKey{}).(HostAccessDecision)
	if !ok {
		return nil
	}
	if decision.Revalidate == nil {
		return api.NewError(http.StatusServiceUnavailable, "access_lease_required",
			"long-lived access lease required", "", nil)
	}
	if err := decision.Revalidate(ctx); err != nil {
		return api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
	}
	return nil
}

func revalidateHostAccess(ctx context.Context) error {
	decision, ok := ctx.Value(hostAccessDecisionContextKey{}).(HostAccessDecision)
	if !ok || decision.Revalidate == nil {
		return nil
	}
	return decision.Revalidate(ctx)
}

func withHostAccess(humaAPI huma.API, controller HostAccessController) {
	if controller == nil {
		return
	}
	humaAPI.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		principal, ok := PrincipalFromContext(ctx.Context())
		if !ok || principal.Kind != PrincipalHost || strings.TrimSpace(principal.Subject) == "" ||
			strings.TrimSpace(principal.Actor) == "" {
			writeHostAccessError(ctx, http.StatusUnauthorized,
				"authentication_required", "authentication required")
			return
		}

		operation := ctx.Operation()
		if operation == nil || strings.TrimSpace(operation.OperationID) == "" {
			writeHostAccessError(ctx, http.StatusServiceUnavailable,
				"access_unavailable", "access decision unavailable")
			return
		}
		pathParams := make(map[string]string)
		for _, parameter := range operation.Parameters {
			if parameter != nil && parameter.In == "path" {
				pathParams[parameter.Name] = ctx.Param(parameter.Name)
			}
		}
		decision, err := controller.Authorize(ctx.Context(), HostAccessRequest{
			Subject: principal.Subject,
			Actor:   principal.Actor,
			Operation: HostOperation{
				ID: operation.OperationID, Method: operation.Method, Path: operation.Path,
				PathParams: pathParams,
			},
		})
		if errors.Is(err, ErrHostAccessDenied) {
			writeHostAccessError(ctx, http.StatusNotFound, "not_found", "resource not found")
			return
		}
		if err != nil {
			writeHostAccessError(ctx, http.StatusServiceUnavailable,
				"access_unavailable", "access decision unavailable")
			return
		}
		next(huma.WithValue(ctx, hostAccessDecisionContextKey{}, decision))
	})
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
	if !ok || principal.Kind != PrincipalHost || strings.TrimSpace(principal.Subject) == "" ||
		strings.TrimSpace(principal.Actor) == "" {
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

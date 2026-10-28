package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/mcpevents"
)

// registerMCPEventsHandlers serves the routes `kata mcp serve` forwards MCP
// Events to. Every route derives the subscription owner from the request's
// authenticated daemon principal, never from the body.
func registerMCPEventsHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "mcpEventsList", Method: http.MethodPost, Path: "/api/v1/mcp-events/list",
		Summary: "List the MCP Events this daemon can deliver",
	}, func(ctx context.Context, _ *struct{}) (*api.MCPEventsListResponse, error) {
		out := &api.MCPEventsListResponse{}
		out.Body.Events = []api.MCPEventsEventDefinition{}
		principal, service, err := mcpEventsCaller(ctx, cfg)
		if err != nil {
			// A forwarder advertises Events whether or not this daemon runs
			// them, so a daemon that has Events off lists no events. Other
			// failures, and every subscribe, still report Unavailable.
			if wire, ok := errors.AsType[*mcpevents.Error](err); ok && wire.Code == mcpevents.CodeUnavailable && wire.Reason == mcpevents.ReasonNotConfigured {
				return out, nil
			}
			return nil, mcpEventsAPIError(err)
		}
		if availability := service.Availability(); !availability.Enabled && availability.Reason == mcpevents.ReasonNotConfigured {
			return out, nil
		}
		listed, err := service.List(ctx, principal)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		for _, event := range listed.Events {
			out.Body.Events = append(out.Body.Events, api.MCPEventsEventDefinition{
				Name: event.Name, Description: event.Description, Delivery: event.Delivery,
				InputSchema: event.InputSchema, PayloadSchema: event.PayloadSchema,
			})
		}
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "mcpEventsSubscribe", Method: http.MethodPost, Path: "/api/v1/mcp-events/subscribe",
		Summary: "Create or refresh an MCP Events webhook subscription",
	}, func(ctx context.Context, in *api.MCPEventsSubscribeRequest) (*api.MCPEventsSubscribeResponse, error) {
		principal, service, err := mcpEventsCaller(ctx, cfg)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		body := in.Body
		// The forwarder scope is advisory narrowing. It is validated here and
		// enforced by the service; the principal's own authority is checked
		// independently, so it can never widen what the principal may see.
		scope := mcpevents.ForwarderScope{All: true}
		if body.ForwarderScope != nil {
			scope, err = mcpevents.NewForwarderScope(body.ForwarderScope.All, body.ForwarderScope.Projects)
			if err != nil {
				return nil, mcpEventsAPIError(err)
			}
		}
		ttl, err := mcpevents.TTLFromWire(body.TTLMS, body.NoExpiry)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		args, err := mcpEventsArguments(body.Arguments)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		result, err := service.Subscribe(ctx, principal, scope, mcpevents.SubscribeRequest{
			Name:      body.Name,
			Arguments: args,
			Delivery:  mcpevents.Delivery{Mode: body.Delivery.Mode, URL: body.Delivery.URL, Secret: body.Delivery.Secret},
			Cursor:    body.Cursor,
			TTLMS:     ttl,
			MaxAgeMS:  body.MaxAgeMS,
		})
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		out := &api.MCPEventsSubscribeResponse{}
		out.Body.ID = result.ID
		out.Body.RefreshBefore = result.RefreshBefore
		out.Body.Cursor = result.Cursor
		out.Body.Truncated = result.Truncated
		out.Body.DeliveryStatus = mcpEventsDeliveryStatus(result.DeliveryStatus)
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "mcpEventsUnsubscribe", Method: http.MethodPost, Path: "/api/v1/mcp-events/unsubscribe",
		Summary: "End an MCP Events webhook subscription",
	}, func(ctx context.Context, in *api.MCPEventsUnsubscribeRequest) (*api.MCPEventsUnsubscribeResponse, error) {
		principal, service, err := mcpEventsCaller(ctx, cfg)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		body := in.Body
		args, err := mcpEventsArguments(body.Arguments)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		err = service.Unsubscribe(ctx, principal, mcpevents.UnsubscribeRequest{
			Name:      body.Name,
			Arguments: args,
			Delivery:  mcpevents.Delivery{Mode: body.Delivery.Mode, URL: body.Delivery.URL},
		})
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		out := &api.MCPEventsUnsubscribeResponse{}
		out.Body.Unsubscribed = true
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "mcpEventSubscriptions", Method: http.MethodGet, Path: "/api/v1/mcp-events/subscriptions",
		Summary: "List the caller's MCP Events subscriptions and whether Events is running",
	}, func(ctx context.Context, _ *struct{}) (*api.MCPEventsSubscriptionsResponse, error) {
		principal, err := MCPEventsPrincipalFromContext(ctx)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		out := &api.MCPEventsSubscriptionsResponse{}
		out.Body.Subscriptions = []api.MCPEventsSubscriptionStatus{}
		if cfg.MCPEvents == nil {
			out.Body.Reason = mcpEventsDisabledReason(cfg)
			return out, nil
		}
		if availability := cfg.MCPEvents.Availability(); !availability.Enabled {
			out.Body.Reason = availability.Reason
			return out, nil
		}
		statuses, err := cfg.MCPEvents.Status(ctx, principal)
		if err != nil {
			return nil, mcpEventsAPIError(err)
		}
		out.Body.Enabled = true
		for _, status := range statuses {
			out.Body.Subscriptions = append(out.Body.Subscriptions, api.MCPEventsSubscriptionStatus{
				ID: status.ID, Name: status.Name, Recipient: status.Recipient,
				ProjectUIDs: status.ProjectUIDs, IssueUID: status.IssueUID,
				State: status.State, EndReason: status.EndReason, RefreshBefore: status.RefreshBefore,
				Cursor: status.Cursor, ContinuityGeneration: status.ContinuityGeneration,
				GapUnacked: status.GapUnacked, Delivery: mcpEventsDeliveryStatus(status.Delivery),
				FailedAttempts: status.FailedAttempts, DeadLetterCount: status.DeadLetterCount,
				PendingControl: status.PendingControl,
			})
		}
		return out, nil
	})
}

// mcpEventsCaller resolves the request's principal and the running service.
// A bootstrap or otherwise non-subscribing principal is Forbidden before the
// availability of Events is revealed.
func mcpEventsCaller(ctx context.Context, cfg ServerConfig) (mcpevents.Principal, *mcpevents.Service, error) {
	principal, err := MCPEventsPrincipalFromContext(ctx)
	if err != nil {
		return mcpevents.Principal{}, nil, err
	}
	if cfg.MCPEvents == nil {
		return mcpevents.Principal{}, nil, mcpEventsUnavailable(mcpEventsDisabledReason(cfg))
	}
	return principal, cfg.MCPEvents, nil
}

func mcpEventsDisabledReason(cfg ServerConfig) string {
	if cfg.MCPEventsDisabledReason != "" {
		return cfg.MCPEventsDisabledReason
	}
	return mcpevents.ReasonNotConfigured
}

func mcpEventsUnavailable(reason string) *mcpevents.Error {
	return &mcpevents.Error{Code: mcpevents.CodeUnavailable, Reason: reason, Data: map[string]any{"reason": reason}}
}

// mcpEventsArguments requires exactly one of a project list and all_projects.
// An empty list is left for the service to reject.
func mcpEventsArguments(a api.MCPEventsArguments) (mcpevents.Arguments, error) {
	if a.AllProjects == (a.ProjectUIDs != nil) {
		return mcpevents.Arguments{}, &mcpevents.Error{Code: mcpevents.CodeInvalidParams, Reason: "invalid_project_uids", Data: map[string]any{"reason": "invalid_project_uids"}}
	}
	return mcpevents.Arguments{For: a.For, ProjectUIDs: a.ProjectUIDs, IssueUID: a.IssueUID}, nil
}

func mcpEventsDeliveryStatus(s mcpevents.DeliveryStatus) api.MCPEventsDeliveryStatus {
	return api.MCPEventsDeliveryStatus{Active: s.Active, LastDeliveryAt: s.LastDeliveryAt, LastError: s.LastError, FailedSince: s.FailedSince}
}

// mcpEventsHTTPStatus maps a §7.2 wire code to the HTTP status of the daemon
// route. The wire code itself travels in the error data.
func mcpEventsHTTPStatus(code int) int {
	switch code {
	case mcpevents.CodeInvalidParams, mcpevents.CodeUnsupported, mcpevents.CodeHeaderMismatch:
		return http.StatusBadRequest
	case mcpevents.CodeNotFound:
		return http.StatusNotFound
	case mcpevents.CodeForbidden:
		return http.StatusForbidden
	case mcpevents.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case mcpevents.CodeCallbackEndpoint:
		return http.StatusBadGateway
	case mcpevents.CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// mcpEventsAPIError renders a service error with its §7.2 code and data, so a
// forwarder can reproduce the protocol error. The message is the safe reason
// only: no URL, secret or transport text can reach it.
func mcpEventsAPIError(err error) error {
	if wire, ok := errors.AsType[*mcpevents.Error](err); ok {
		data := map[string]any{api.MCPEventsErrorCodeKey: wire.Code}
		wireData := wire.Data
		if wireData == nil {
			wireData = map[string]any{"reason": wire.Reason}
		}
		data[api.MCPEventsErrorDataKey] = wireData
		return api.NewError(mcpEventsHTTPStatus(wire.Code), "mcp_events", "MCP Events: "+wire.Reason, "", data)
	}
	return internalAPIError(err)
}

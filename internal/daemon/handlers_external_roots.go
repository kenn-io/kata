package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	connectorclient "go.kenn.io/kata/internal/connector"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/rootbridge"
	"go.kenn.io/kata/pkg/connector"
)

var externalRootOperationIDs = map[string]bool{
	"listConnectors": true, "getConnectorStatus": true, "listConnectorFields": true,
	"mapConnectorField": true, "unmapConnectorField": true, "bindExternalRoot": true,
	"getExternalRootBridge": true, "reconcileExternalRootBridge": true,
	"reconcileExternalRootByKey": true, "pauseExternalRootBridge": true,
	"resumeExternalRootBridge": true, "resolveExternalField": true,
	"resolveExternalComment": true, "unbindExternalRoot": true,
}

func withExternalRootAdministration(humaAPI huma.API, allowIdentityConnectorAdministration bool) {
	humaAPI.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		operation := ctx.Operation()
		if operation == nil || !externalRootOperationIDs[operation.OperationID] {
			next(ctx)
			return
		}
		if err := ensureExternalRootAdministrationAllowed(ctx.Context(), allowIdentityConnectorAdministration); err != nil {
			writeHostAccessError(ctx, http.StatusForbidden, "integration_administration_forbidden", "connector administration is unavailable to this principal")
			return
		}
		next(ctx)
	})
}

func ensureExternalRootAdministrationAllowed(ctx context.Context, allowIdentityConnectorAdministration bool) error {
	if webSessionAuthenticated(ctx) || insecureReadonlyRequest(ctx) || unauthenticatedPrivateNetworkRequest(ctx) {
		return api.NewError(http.StatusForbidden, "integration_administration_forbidden", "connector administration is unavailable to this principal", "", nil)
	}
	if principal, ok := PrincipalFromContext(ctx); ok {
		if principal.Kind == PrincipalDBToken && allowIdentityConnectorAdministration {
			return nil
		}
		switch principal.Kind {
		case PrincipalDBToken, PrincipalBootstrap, PrincipalTrustedProxy,
			PrincipalTrustedProxyAbsent, PrincipalWebLocal:
			return api.NewError(http.StatusForbidden, "integration_administration_forbidden", "connector administration is unavailable to this principal", "", nil)
		}
	}
	return nil
}

func registerExternalRootHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "listConnectors", Method: http.MethodGet, Path: "/api/v1/connectors"},
		func(ctx context.Context, _ *struct{}) (*api.ConnectorListResponse, error) {
			response := &api.ConnectorListResponse{}
			if cfg.ExternalRootRegistry == nil {
				response.Body.Connectors = []api.ConnectorOut{}
				return response, nil
			}
			for _, snapshot := range cfg.ExternalRootRegistry.RefreshSnapshots(ctx) {
				response.Body.Connectors = append(response.Body.Connectors, connectorOut(snapshot))
			}
			return response, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "getConnectorStatus", Method: http.MethodGet, Path: "/api/v1/connectors/{instance}"},
		func(ctx context.Context, in *api.ConnectorPathRequest) (*api.ConnectorResponse, error) {
			if cfg.ExternalRootRegistry == nil {
				return nil, externalRootHandlerError(rootbridge.ErrConnectorInstanceNotFound)
			}
			snapshot, err := cfg.ExternalRootRegistry.Refresh(ctx, in.Instance)
			if err != nil {
				if safe, ok := cfg.ExternalRootRegistry.Snapshot(in.Instance); ok {
					return &api.ConnectorResponse{Body: connectorOut(safe)}, nil
				}
				return nil, externalRootHandlerError(err)
			}
			return &api.ConnectorResponse{Body: connectorOut(snapshot)}, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "listConnectorFields", Method: http.MethodGet, Path: "/api/v1/connectors/{instance}/fields"},
		func(ctx context.Context, in *api.ConnectorPathRequest) (*api.ConnectorFieldsResponse, error) {
			if cfg.ExternalRootRegistry == nil {
				return nil, externalRootHandlerError(rootbridge.ErrConnectorInstanceNotFound)
			}
			fields, err := cfg.ExternalRootRegistry.Fields(ctx, in.Instance)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ConnectorFieldsResponse{Body: struct {
				Fields []connector.FieldDescriptor `json:"fields"`
			}{Fields: fields}}, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "mapConnectorField", Method: http.MethodPut, Path: "/api/v1/connectors/{instance}/fields/{kata_field}"},
		func(ctx context.Context, in *api.MapConnectorFieldRequest) (*api.ConnectorFieldMappingResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			if cfg.ExternalRootService == nil {
				return nil, externalRootUnavailable()
			}
			mapping, err := cfg.ExternalRootService.MapField(ctx, rootbridge.MapFieldParams{
				ConnectorInstance: in.Instance, KataField: in.KataField, ExternalField: in.Body.ExternalField,
			})
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ConnectorFieldMappingResponse{Body: api.ExternalFieldMappingFromDB(mapping)}, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "unmapConnectorField", Method: http.MethodDelete, Path: "/api/v1/connectors/{instance}/fields/{kata_field}"},
		func(ctx context.Context, in *api.UnmapConnectorFieldRequest) (*api.ConnectorFieldMappingResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			if cfg.ExternalRootService == nil {
				return nil, externalRootUnavailable()
			}
			mapping, err := cfg.ExternalRootService.UnmapField(ctx, in.Instance, in.KataField)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ConnectorFieldMappingResponse{Body: api.ExternalFieldMappingFromDB(mapping)}, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "bindExternalRoot", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/issues/{ref}/bridge"},
		func(ctx context.Context, in *api.BindExternalRootRequest) (*api.ExternalRootBridgeResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			if cfg.ExternalRootService == nil {
				return nil, externalRootUnavailable()
			}
			issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
			actor, err := attributedActor(ctx, in.Body.Actor)
			if err != nil {
				return nil, err
			}
			binding, _, callErr := cfg.ExternalRootService.Bind(ctx, rootbridge.BindParams{
				ProjectID: in.ProjectID, IssueID: issue.ID, ConnectorInstance: in.Body.Connector,
				Locator: in.Body.External, Actor: actor, PublishComments: in.Body.PublishComments,
			})
			if binding.ID != 0 && cfg.ExternalRootWake != nil {
				cfg.ExternalRootWake(binding.ID)
			}
			if callErr != nil {
				return nil, externalRootHandlerError(callErr)
			}
			out, err := externalRootBridgeOut(ctx, cfg.DB, binding)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ExternalRootBridgeResponse{Body: out}, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "getExternalRootBridge", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/issues/{ref}/bridge"},
		func(ctx context.Context, in *api.ExternalRootIssueRequest) (*api.ExternalRootBridgeResponse, error) {
			issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
			binding, err := cfg.DB.ExternalRootBindingByIssue(ctx, issue.ID)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			out, err := externalRootBridgeOut(ctx, cfg.DB, binding)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ExternalRootBridgeResponse{Body: out}, nil
		})

	registerExternalRootManualReconcile(humaAPI, cfg)
	registerExternalRootActions(humaAPI, cfg)
}

func registerExternalRootManualReconcile(humaAPI huma.API, cfg ServerConfig) {
	run := func(ctx context.Context, binding db.ExternalRootBinding) (*api.ExternalRootRunResponse, error) {
		if cfg.ExternalRootReconciler == nil {
			return nil, externalRootUnavailable()
		}
		result, runErr := cfg.ExternalRootReconciler.Run(ctx, binding.ID, rootbridge.RunOptions{
			Manual: true,
			EventSink: func(event db.Event) {
				deliverExternalRootEvents(cfg, []db.Event{event})
			},
		})
		if runErr != nil {
			return nil, externalRootHandlerError(runErr)
		}
		current, err := cfg.DB.ExternalRootBindingByID(ctx, binding.ID)
		if err != nil {
			return nil, externalRootHandlerError(err)
		}
		out, err := externalRootBridgeOut(ctx, cfg.DB, current)
		if err != nil {
			return nil, externalRootHandlerError(err)
		}
		return &api.ExternalRootRunResponse{Body: runResultOut(out, result)}, nil
	}
	huma.Register(humaAPI, huma.Operation{OperationID: "reconcileExternalRootBridge", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/issues/{ref}/bridge/actions/reconcile"},
		func(ctx context.Context, in *api.ExternalRootActionRequest) (*api.ExternalRootRunResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
			binding, err := cfg.DB.ExternalRootBindingByIssue(ctx, issue.ID)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return run(ctx, binding)
		})
	huma.Register(humaAPI, huma.Operation{OperationID: "reconcileExternalRootByKey", Method: http.MethodPost, Path: "/api/v1/connectors/{instance}/actions/reconcile-root"},
		func(ctx context.Context, in *api.ReconcileExternalRootByKeyRequest) (*api.ExternalRootRunResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.Body.RootKey) == "" {
				return nil, api.NewError(http.StatusBadRequest, "validation", "root key is required", "", nil)
			}
			binding, err := cfg.DB.ExternalRootBindingByExternalKey(ctx, in.Instance, in.Body.RootKey)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return run(ctx, binding)
		})
}

func registerExternalRootActions(humaAPI huma.API, cfg ServerConfig) {
	issueBinding := func(ctx context.Context, projectID int64, ref string) (db.Issue, db.ExternalRootBinding, error) {
		issue, err := activeIssueByRef(ctx, cfg.DB, projectID, ref, db.IncludeDeletedNo)
		if err != nil {
			return db.Issue{}, db.ExternalRootBinding{}, err
		}
		binding, err := cfg.DB.ExternalRootBindingByIssue(ctx, issue.ID)
		if err != nil {
			return db.Issue{}, db.ExternalRootBinding{}, externalRootHandlerError(err)
		}
		if binding.ProjectID != projectID || binding.IssueID != issue.ID {
			return db.Issue{}, db.ExternalRootBinding{}, api.NewError(http.StatusNotFound, "bridge_not_found", "external root bridge not found", "", nil)
		}
		return issue, binding, nil
	}
	registerAction := func(operationID, path string, action func(context.Context, int64, string, string) (db.ExternalRootBinding, db.Event, error)) {
		huma.Register(humaAPI, huma.Operation{OperationID: operationID, Method: http.MethodPost, Path: path},
			func(ctx context.Context, in *api.ExternalRootActionRequest) (*api.ExternalRootBridgeResponse, error) {
				if err := ensureAttributedWriteAllowed(ctx); err != nil {
					return nil, err
				}
				if cfg.ExternalRootService == nil {
					return nil, externalRootUnavailable()
				}
				issue, _, err := issueBinding(ctx, in.ProjectID, in.Ref)
				if err != nil {
					return nil, err
				}
				actor, err := attributedActor(ctx, in.Body.Actor)
				if err != nil {
					return nil, err
				}
				binding, _, err := action(ctx, issue.ID, actor, in.Body.Reason)
				if err != nil {
					return nil, externalRootHandlerError(err)
				}
				if cfg.ExternalRootWake != nil {
					cfg.ExternalRootWake(binding.ID)
				}
				out, err := externalRootBridgeOut(ctx, cfg.DB, binding)
				if err != nil {
					return nil, externalRootHandlerError(err)
				}
				return &api.ExternalRootBridgeResponse{Body: out}, nil
			})
	}
	registerAction("pauseExternalRootBridge", "/api/v1/projects/{project_id}/issues/{ref}/bridge/actions/pause",
		func(ctx context.Context, issueID int64, actor, reason string) (db.ExternalRootBinding, db.Event, error) {
			return cfg.ExternalRootService.Pause(ctx, issueID, actor, reason)
		})
	registerAction("resumeExternalRootBridge", "/api/v1/projects/{project_id}/issues/{ref}/bridge/actions/resume",
		func(ctx context.Context, issueID int64, actor, _ string) (db.ExternalRootBinding, db.Event, error) {
			return cfg.ExternalRootService.Resume(ctx, issueID, actor)
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "resolveExternalField", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/issues/{ref}/bridge/actions/resolve-field"},
		func(ctx context.Context, in *api.ResolveExternalFieldRequest) (*api.ExternalRootBridgeResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			if cfg.ExternalRootService == nil {
				return nil, externalRootUnavailable()
			}
			issue, binding, err := issueBinding(ctx, in.ProjectID, in.Ref)
			if err != nil {
				return nil, err
			}
			actor, err := attributedActor(ctx, in.Body.Actor)
			if err != nil {
				return nil, err
			}
			_, _, callErr := cfg.ExternalRootService.ResolveFieldConflict(ctx, issue.ID, in.Body.KataField, in.Body.Use, actor)
			if callErr != nil {
				return nil, externalRootHandlerError(callErr)
			}
			if cfg.ExternalRootWake != nil {
				cfg.ExternalRootWake(binding.ID)
			}
			current, err := cfg.DB.ExternalRootBindingByID(ctx, binding.ID)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			out, err := externalRootBridgeOut(ctx, cfg.DB, current)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ExternalRootBridgeResponse{Body: out}, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "resolveExternalComment", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/issues/{ref}/bridge/actions/resolve-comment"},
		func(ctx context.Context, in *api.ResolveExternalCommentRequest) (*api.ExternalRootBridgeResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			if cfg.ExternalRootService == nil {
				return nil, externalRootUnavailable()
			}
			// Pending-comment resolution is lifecycle cleanup, exactly like
			// unbind: it must remain available after project archival so an
			// operator can clear the pending state, unbind, and satisfy the
			// project-purge guard without restoring the project.
			issue, err := resolveIssueRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
			binding, err := cfg.DB.ExternalRootBindingByIssue(ctx, issue.ID)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			if binding.ProjectID != in.ProjectID || binding.IssueID != issue.ID {
				return nil, api.NewError(http.StatusNotFound, "bridge_not_found", "external root bridge not found", "", nil)
			}
			actor, err := attributedActor(ctx, in.Body.Actor)
			if err != nil {
				return nil, err
			}
			_, _, callErr := cfg.ExternalRootService.ResolvePendingComment(ctx, rootbridge.ResolvePendingCommentParams{
				IssueID: issue.ID, Actor: actor, Action: in.Body.Action, ExternalCommentID: in.Body.ExternalCommentID,
			})
			if callErr != nil {
				return nil, externalRootHandlerError(callErr)
			}
			if cfg.ExternalRootWake != nil {
				cfg.ExternalRootWake(binding.ID)
			}
			current, err := cfg.DB.ExternalRootBindingByID(ctx, binding.ID)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			out, err := externalRootBridgeOut(ctx, cfg.DB, current)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ExternalRootBridgeResponse{Body: out}, nil
		})

	huma.Register(humaAPI, huma.Operation{OperationID: "unbindExternalRoot", Method: http.MethodDelete, Path: "/api/v1/projects/{project_id}/issues/{ref}/bridge"},
		func(ctx context.Context, in *api.ExternalRootIssueRequest) (*api.ExternalRootBridgeResponse, error) {
			if err := ensureAttributedWriteAllowed(ctx); err != nil {
				return nil, err
			}
			if cfg.ExternalRootService == nil {
				return nil, externalRootUnavailable()
			}
			// Unbind is lifecycle cleanup and must remain available after project
			// archival so an operator can satisfy the project-purge guard.
			issue, err := resolveIssueRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
			if err != nil {
				return nil, err
			}
			binding, err := cfg.DB.ExternalRootBindingByIssue(ctx, issue.ID)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			if binding.ProjectID != in.ProjectID || binding.IssueID != issue.ID {
				return nil, api.NewError(http.StatusNotFound, "bridge_not_found", "external root bridge not found", "", nil)
			}
			actor, err := attributedActor(ctx, in.Actor)
			if err != nil {
				return nil, err
			}
			binding, _, err = cfg.ExternalRootService.Unbind(ctx, issue.ID, actor)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			out, err := externalRootBridgeOut(ctx, cfg.DB, binding)
			if err != nil {
				return nil, externalRootHandlerError(err)
			}
			return &api.ExternalRootBridgeResponse{Body: out}, nil
		})
}

func connectorOut(snapshot rootbridge.InstanceSnapshot) api.ConnectorOut {
	return api.ConnectorOut{
		InstanceID: snapshot.ID, ConnectorID: snapshot.ConnectorID, DisplayName: snapshot.DisplayName,
		Protocol: snapshot.Protocol, AccountIdentity: snapshot.AccountIdentity,
		Capabilities: append([]connector.Capability(nil), snapshot.Capabilities...),
		Healthy:      snapshot.HealthError == "" && snapshot.ConnectorID != "", HealthError: snapshot.HealthError,
	}
}

func externalRootBridgeOut(ctx context.Context, store db.Storage, binding db.ExternalRootBinding) (api.ExternalRootBridgeOut, error) {
	out := api.ExternalRootBridgeOut{
		ID: binding.ID, UID: binding.UID, ProjectID: binding.ProjectID, IssueID: binding.IssueID,
		ConnectorInstance: binding.ConnectorInstance, Active: binding.Active, Enabled: binding.Enabled,
		ReceiveComments: binding.ReceiveComments, PublishComments: binding.PublishComments,
		CompleteExternal: binding.CompleteExternal, PausedReason: binding.PausedReason,
		LastAttemptAt: binding.LastAttemptAt, LastSuccessAt: binding.LastSuccessAt,
		LastErrorAt: binding.LastErrorAt, LastError: db.SafeExternalRootError(binding.LastError),
		ConsecutiveFailures: binding.ConsecutiveFailures, NextAttemptAt: binding.NextAttemptAt,
		LastExternalState: binding.LastExternalState, PendingCommentUID: binding.PendingCommentUID,
	}
	states, err := store.ExternalFieldStates(ctx, binding.ID)
	if err != nil {
		return api.ExternalRootBridgeOut{}, err
	}
	mappings, err := store.ListExternalFieldMappings(ctx, binding.ConnectorInstance)
	if err != nil {
		return api.ExternalRootBridgeOut{}, err
	}
	byID := make(map[int64]string, len(mappings))
	for _, mapping := range mappings {
		if mapping.Active {
			byID[mapping.ID] = mapping.KataField
		}
	}
	for _, state := range states {
		kataField, active := byID[state.MappingID]
		if !state.Conflicted || !active {
			continue
		}
		kataCandidate, err := externalFieldCandidateOut(state.ConflictKata)
		if err != nil {
			return api.ExternalRootBridgeOut{}, err
		}
		externalCandidate, err := externalFieldCandidateOut(state.ConflictExternal)
		if err != nil {
			return api.ExternalRootBridgeOut{}, err
		}
		out.FieldConflicts = append(out.FieldConflicts, api.ExternalFieldConflictOut{
			KataField: kataField, KataCandidate: kataCandidate,
			ExternalCandidate: externalCandidate, ConflictAt: state.ConflictAt,
		})
	}
	return out, nil
}

func externalFieldCandidateOut(raw json.RawMessage) (*api.ExternalFieldCandidateOut, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var candidate api.ExternalFieldCandidateOut
	if err := json.Unmarshal(raw, &candidate); err != nil || strings.TrimSpace(candidate.Kind) == "" {
		return nil, fmt.Errorf("decode external field conflict candidate")
	}
	return &candidate, nil
}

func runResultOut(bridge api.ExternalRootBridgeOut, result rootbridge.RunResult) api.ExternalRootRunOut {
	return api.ExternalRootRunOut{
		Bridge: bridge, RootUpdated: result.RootUpdated, CommentsCreated: result.CommentsCreated,
		CommentsEdited: result.CommentsEdited, FieldConflicts: result.FieldConflicts,
		CompletionRequests: result.CompletionRequests, ReopenRequests: result.ReopenRequests, Paused: result.Paused,
	}
}

func deliverExternalRootEvents(cfg ServerConfig, events []db.Event) {
	for i := range events {
		event := events[i]
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &event, ProjectID: event.ProjectID})
		cfg.Hooks.Enqueue(event)
	}
}

func externalRootUnavailable() error {
	return api.NewError(http.StatusServiceUnavailable, "connector_unavailable", "external root connector service is unavailable", "", nil)
}

func externalRootHandlerError(err error) error {
	switch {
	case errors.Is(err, db.ErrNotFound), errors.Is(err, rootbridge.ErrConnectorInstanceNotFound):
		return api.NewError(http.StatusNotFound, "bridge_not_found", "external root bridge not found", "", nil)
	case errors.Is(err, db.ErrExternalRootValidation), errors.Is(err, db.ErrExternalFieldMappingValidation):
		return api.NewError(http.StatusBadRequest, "validation", "external root request is invalid", "", nil)
	case errors.Is(err, rootbridge.ErrExternalFieldAmbiguous):
		return api.NewError(http.StatusBadRequest, "validation", "external root request is invalid", "", nil)
	case errors.Is(err, rootbridge.ErrExternalFieldNotFound):
		return api.NewError(http.StatusNotFound, "external_field_not_found", "external field was not found", "", nil)
	case errors.Is(err, db.ErrFederatedReadOnly):
		return federationReadOnlyError(db.ErrFederatedReadOnly)
	case errors.Is(err, db.ErrExternalRootIssueAlreadyBound),
		errors.Is(err, db.ErrExternalRootAlreadyBound),
		errors.Is(err, db.ErrExternalRootIssueSyncConflict),
		errors.Is(err, db.ErrExternalRootClaimActive),
		errors.Is(err, rootbridge.ErrConnectorIdentityChanged):
		return api.NewError(http.StatusConflict, "external_root_conflict", "external root operation conflicts with an existing binding or connector identity", "", nil)
	case errors.Is(err, db.ErrExternalRootClaimLost):
		return api.NewError(http.StatusConflict, "external_root_claim_lost", "external root bridge is busy or unavailable", "", nil)
	case errors.Is(err, rootbridge.ErrPendingCommentResolutionRequired), errors.Is(err, db.ErrExternalCommentPending):
		return api.NewError(http.StatusConflict, "external_comment_pending", "external comment requires explicit resolution", "", nil)
	case errors.Is(err, rootbridge.ErrFieldConflictChanged), errors.Is(err, db.ErrExternalFieldNotConflicted):
		return api.NewError(http.StatusConflict, "external_field_conflict", "external field conflict changed", "", nil)
	case errors.Is(err, rootbridge.ErrCommentPublishingUnavailable),
		errors.Is(err, rootbridge.ErrFieldSynchronizationUnavailable):
		return api.NewError(http.StatusConflict, "external_root_conflict", "external root operation conflicts with connector capabilities", "", nil)
	case errors.Is(err, connectorclient.ErrRequestTimeout):
		return api.NewError(http.StatusBadGateway, "connector_error", connectorclient.ErrRequestTimeout.Error(), "", nil)
	case errors.Is(err, connectorclient.ErrProtocolFailure):
		return api.NewError(http.StatusBadGateway, "connector_error", connectorclient.ErrProtocolFailure.Error(), "", nil)
	case errors.Is(err, connectorclient.ErrProcessFailure):
		return api.NewError(http.StatusBadGateway, "connector_error", connectorclient.ErrProcessFailure.Error(), "", nil)
	case errors.Is(err, rootbridge.ErrConnectorCall):
		return api.NewError(http.StatusBadGateway, "connector_error", "external connector request failed", "", nil)
	default:
		if _, ok := errors.AsType[*connector.Error](err); ok {
			return api.NewError(http.StatusBadGateway, "connector_error", "external connector request failed", "", nil)
		}
		return api.NewError(http.StatusInternalServerError, "internal", "internal server error", "", nil)
	}
}

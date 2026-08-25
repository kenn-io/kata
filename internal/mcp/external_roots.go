package mcpserver

import (
	"context"
	"errors"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// ConnectorsInput optionally selects one configured connector instance.
type ConnectorsInput struct {
	Instance string `json:"instance,omitempty"`
}

// ConnectorsOutput contains secret-free connector status records.
type ConnectorsOutput struct {
	Connectors []generated.ConnectorOut `json:"connectors"`
}

// ConnectorFieldsInput selects one configured connector instance.
type ConnectorFieldsInput struct {
	Instance string `json:"instance"`
}

// ConnectorFieldsOutput contains one connector's canonical field descriptors.
type ConnectorFieldsOutput struct {
	Instance string                      `json:"instance"`
	Fields   []generated.FieldDescriptor `json:"fields"`
}

// ConnectorFieldMapInput maps one Kata planning field to an external field.
type ConnectorFieldMapInput struct {
	Instance      string `json:"instance"`
	KataField     string `json:"kata_field"`
	ExternalField string `json:"external_field"`
}

// ConnectorFieldUnmapInput disables one connector planning-field mapping.
type ConnectorFieldUnmapInput struct {
	Instance  string `json:"instance"`
	KataField string `json:"kata_field"`
}

// ConnectorFieldMappingOutput reports the connector-wide mapping state.
type ConnectorFieldMappingOutput struct {
	Instance string                            `json:"instance"`
	Mapping  generated.ExternalFieldMappingOut `json:"mapping"`
}

// BridgeInput selects one issue bridge inside the startup scope.
type BridgeInput struct {
	Ref string `json:"ref"`
}

// BridgeBindInput binds an issue to an existing external root.
type BridgeBindInput struct {
	Ref             string `json:"ref"`
	Connector       string `json:"connector"`
	External        string `json:"external"`
	PublishComments bool   `json:"publish_comments,omitempty"`
}

// BridgePauseInput pauses an issue bridge with an optional safe reason.
type BridgePauseInput struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason,omitempty"`
}

// BridgeResolveFieldInput selects the winner for one planning-field conflict.
type BridgeResolveFieldInput struct {
	Ref       string `json:"ref"`
	KataField string `json:"kata_field"`
	Use       string `json:"use"`
}

// BridgeResolveCommentInput resolves uncertain outbound comment delivery.
type BridgeResolveCommentInput struct {
	Ref               string `json:"ref"`
	Action            string `json:"action"`
	ExternalCommentID string `json:"external_comment_id,omitempty"`
}

// BridgeOutput reports one issue bridge inside its current project.
type BridgeOutput struct {
	Project ProjectIdentity                 `json:"project"`
	Bridge  generated.ExternalRootBridgeOut `json:"bridge"`
}

// BridgeReconcileOutput reports one completed reconciliation pass.
type BridgeReconcileOutput struct {
	Project ProjectIdentity              `json:"project"`
	Result  generated.ExternalRootRunOut `json:"result"`
}

func registerExternalRootTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	openWorldRead := toolHints(true, false, true)
	mutating := toolHints(false, true, false)
	openWorld := toolHints(false, true, true)
	addTool(server, "kata.connectors", "Connectors", "List configured external connectors or read one secret-free status; requires daemon-wide scope.", openWorldRead, handlers.connectors)
	addTool(server, "kata.connector_fields", "Connector fields", "Read canonical external field descriptors for one configured connector; requires daemon-wide scope.", openWorldRead, handlers.connectorFields)
	addTool(server, "kata.connector_field_map", "Map connector field", "Map one Kata planning field to a canonical external field; requires daemon-wide scope.", openWorld, handlers.connectorFieldMap)
	addTool(server, "kata.connector_field_unmap", "Unmap connector field", "Disable one connector planning-field mapping; requires daemon-wide scope.", openWorld, handlers.connectorFieldUnmap)
	addTool(server, "kata.bridge_show", "Show bridge", "Read external-root bridge status for one issue.", read, handlers.bridgeShow)
	addTool(server, "kata.bridge_bind", "Bind bridge", "Bind one issue to an existing external root; requires daemon-wide scope.", openWorld, handlers.bridgeBind)
	addTool(server, "kata.bridge_reconcile", "Reconcile bridge", "Run one external-root reconciliation pass.", nonIdempotent(toolHints(false, true, true)), handlers.bridgeReconcile)
	addTool(server, "kata.bridge_pause", "Pause bridge", "Pause external-root reconciliation for one issue.", mutating, handlers.bridgePause)
	addTool(server, "kata.bridge_resume", "Resume bridge", "Validate and resume external-root reconciliation for one issue.", openWorld, handlers.bridgeResume)
	addTool(server, "kata.bridge_resolve_field", "Resolve bridge field", "Choose the Kata or external candidate for one planning-field conflict.", openWorld, handlers.bridgeResolveField)
	addTool(server, "kata.bridge_resolve_comment", "Resolve bridge comment", "Adopt, retry, or skip uncertain outbound comment delivery.", openWorld, handlers.bridgeResolveComment)
	addTool(server, "kata.bridge_unbind", "Unbind bridge", "Deactivate one issue bridge while preserving its history.", mutating, handlers.bridgeUnbind)
}

func (h toolHandlers) connectors(ctx context.Context, _ *sdkmcp.CallToolRequest, input ConnectorsInput) (*sdkmcp.CallToolResult, ConnectorsOutput, error) {
	if err := h.requireAllProjectsScope("connector metadata"); err != nil {
		return nil, ConnectorsOutput{}, err
	}
	h = h.withLongRunningClient()
	instance := strings.TrimSpace(input.Instance)
	if instance == "" {
		response, err := h.options.Client.ListConnectors(ctx)
		if err != nil {
			return nil, ConnectorsOutput{}, err
		}
		return successResult(), ConnectorsOutput{Connectors: response.Connectors}, nil
	}
	response, err := h.options.Client.GetConnectorStatus(ctx, &generated.GetConnectorStatusRequestOptions{
		PathParams: &generated.GetConnectorStatusPath{Instance: instance},
	})
	if err != nil {
		return nil, ConnectorsOutput{}, err
	}
	return successResult(), ConnectorsOutput{Connectors: []generated.ConnectorOut{*response}}, nil
}

func (h toolHandlers) connectorFields(ctx context.Context, _ *sdkmcp.CallToolRequest, input ConnectorFieldsInput) (*sdkmcp.CallToolResult, ConnectorFieldsOutput, error) {
	if err := h.requireAllProjectsScope("connector field metadata"); err != nil {
		return nil, ConnectorFieldsOutput{}, err
	}
	h = h.withLongRunningClient()
	instance := strings.TrimSpace(input.Instance)
	if instance == "" {
		return nil, ConnectorFieldsOutput{}, errors.New("connector instance must not be empty")
	}
	response, err := h.options.Client.ListConnectorFields(ctx, &generated.ListConnectorFieldsRequestOptions{
		PathParams: &generated.ListConnectorFieldsPath{Instance: instance},
	})
	if err != nil {
		return nil, ConnectorFieldsOutput{}, err
	}
	return successResult(), ConnectorFieldsOutput{Instance: instance, Fields: response.Fields}, nil
}

func (h toolHandlers) connectorFieldMap(ctx context.Context, _ *sdkmcp.CallToolRequest, input ConnectorFieldMapInput) (*sdkmcp.CallToolResult, ConnectorFieldMappingOutput, error) {
	if err := h.requireAllProjectsScope("connector field mapping"); err != nil {
		return nil, ConnectorFieldMappingOutput{}, err
	}
	instance, kataField, err := connectorMappingTarget(input.Instance, input.KataField)
	if err != nil {
		return nil, ConnectorFieldMappingOutput{}, err
	}
	externalField := strings.TrimSpace(input.ExternalField)
	if externalField == "" {
		return nil, ConnectorFieldMappingOutput{}, errors.New("external field must not be empty")
	}
	h = h.withLongRunningClient()
	response, err := h.options.Client.MapConnectorField(ctx, &generated.MapConnectorFieldRequestOptions{
		PathParams: &generated.MapConnectorFieldPath{Instance: instance, KataField: kataField},
		Body:       &generated.MapConnectorFieldBody{ExternalField: externalField},
	})
	if err != nil {
		return nil, ConnectorFieldMappingOutput{}, err
	}
	return successResult(), ConnectorFieldMappingOutput{Instance: instance, Mapping: *response}, nil
}

func (h toolHandlers) connectorFieldUnmap(ctx context.Context, _ *sdkmcp.CallToolRequest, input ConnectorFieldUnmapInput) (*sdkmcp.CallToolResult, ConnectorFieldMappingOutput, error) {
	if err := h.requireAllProjectsScope("connector field unmapping"); err != nil {
		return nil, ConnectorFieldMappingOutput{}, err
	}
	instance, kataField, err := connectorMappingTarget(input.Instance, input.KataField)
	if err != nil {
		return nil, ConnectorFieldMappingOutput{}, err
	}
	h = h.withLongRunningClient()
	response, err := h.options.Client.UnmapConnectorField(ctx, &generated.UnmapConnectorFieldRequestOptions{
		PathParams: &generated.UnmapConnectorFieldPath{Instance: instance, KataField: kataField},
	})
	if err != nil {
		return nil, ConnectorFieldMappingOutput{}, err
	}
	return successResult(), ConnectorFieldMappingOutput{Instance: instance, Mapping: *response}, nil
}

func connectorMappingTarget(rawInstance, rawKataField string) (string, string, error) {
	instance := strings.TrimSpace(rawInstance)
	kataField := strings.TrimSpace(rawKataField)
	if instance == "" {
		return "", "", errors.New("connector instance must not be empty")
	}
	if kataField != "scheduled_on" && kataField != "deadline_on" {
		return "", "", errors.New("kata field must be scheduled_on or deadline_on")
	}
	return instance, kataField, nil
}

func (h toolHandlers) bridgeShow(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgeInput) (*sdkmcp.CallToolResult, BridgeOutput, error) {
	project, ref, err := h.bridgeTarget(ctx, input.Ref, false)
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	response, err := h.options.Client.GetExternalRootBridge(ctx, &generated.GetExternalRootBridgeRequestOptions{
		PathParams: &generated.GetExternalRootBridgePath{ProjectID: project.ID, Ref: ref},
	})
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	return successResult(), BridgeOutput{Project: project, Bridge: *response}, nil
}

func (h toolHandlers) bridgeBind(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgeBindInput) (*sdkmcp.CallToolResult, BridgeOutput, error) {
	if err := h.requireAllProjectsScope("external-root bridge binding"); err != nil {
		return nil, BridgeOutput{}, err
	}
	project, ref, err := h.bridgeTarget(ctx, input.Ref, true)
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	connectorInstance := strings.TrimSpace(input.Connector)
	external := strings.TrimSpace(input.External)
	if connectorInstance == "" || external == "" {
		return nil, BridgeOutput{}, errors.New("connector and external root must not be empty")
	}
	h = h.withLongRunningClient()
	response, err := h.options.Client.BindExternalRoot(ctx, &generated.BindExternalRootRequestOptions{
		PathParams: &generated.BindExternalRootPath{ProjectID: project.ID, Ref: ref},
		Body: &generated.BindExternalRootBody{
			Actor: &h.options.Actor, Connector: connectorInstance, External: external, PublishComments: &input.PublishComments,
		},
	})
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	return successResult(), BridgeOutput{Project: project, Bridge: *response}, nil
}

func (h toolHandlers) bridgeReconcile(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgeInput) (*sdkmcp.CallToolResult, BridgeReconcileOutput, error) {
	project, ref, err := h.bridgeTarget(ctx, input.Ref, true)
	if err != nil {
		return nil, BridgeReconcileOutput{}, err
	}
	h = h.withLongRunningClient()
	response, err := h.options.Client.ReconcileExternalRootBridge(ctx, &generated.ReconcileExternalRootBridgeRequestOptions{
		PathParams: &generated.ReconcileExternalRootBridgePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.ReconcileExternalRootBridgeBody{Actor: &h.options.Actor},
	})
	if err != nil {
		return nil, BridgeReconcileOutput{}, err
	}
	return successResult(), BridgeReconcileOutput{Project: project, Result: *response}, nil
}

func (h toolHandlers) bridgePause(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgePauseInput) (*sdkmcp.CallToolResult, BridgeOutput, error) {
	project, ref, err := h.bridgeTarget(ctx, input.Ref, true)
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	response, err := h.options.Client.PauseExternalRootBridge(ctx, &generated.PauseExternalRootBridgeRequestOptions{
		PathParams: &generated.PauseExternalRootBridgePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.PauseExternalRootBridgeBody{Actor: &h.options.Actor, Reason: optionalString(input.Reason)},
	})
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	return successResult(), BridgeOutput{Project: project, Bridge: *response}, nil
}

func (h toolHandlers) bridgeResume(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgeInput) (*sdkmcp.CallToolResult, BridgeOutput, error) {
	project, ref, err := h.bridgeTarget(ctx, input.Ref, true)
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	h = h.withLongRunningClient()
	response, err := h.options.Client.ResumeExternalRootBridge(ctx, &generated.ResumeExternalRootBridgeRequestOptions{
		PathParams: &generated.ResumeExternalRootBridgePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.ResumeExternalRootBridgeBody{Actor: &h.options.Actor},
	})
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	return successResult(), BridgeOutput{Project: project, Bridge: *response}, nil
}

func (h toolHandlers) bridgeResolveField(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgeResolveFieldInput) (*sdkmcp.CallToolResult, BridgeOutput, error) {
	project, ref, err := h.bridgeTarget(ctx, input.Ref, true)
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	kataField := strings.TrimSpace(input.KataField)
	use := strings.TrimSpace(input.Use)
	if kataField != "scheduled_on" && kataField != "deadline_on" {
		return nil, BridgeOutput{}, errors.New("kata field must be scheduled_on or deadline_on")
	}
	if use != "kata" && use != "external" {
		return nil, BridgeOutput{}, errors.New("use must be kata or external")
	}
	h = h.withLongRunningClient()
	response, err := h.options.Client.ResolveExternalField(ctx, &generated.ResolveExternalFieldRequestOptions{
		PathParams: &generated.ResolveExternalFieldPath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.ResolveExternalFieldBody{Actor: &h.options.Actor, KataField: kataField, Use: use},
	})
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	return successResult(), BridgeOutput{Project: project, Bridge: *response}, nil
}

func (h toolHandlers) bridgeResolveComment(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgeResolveCommentInput) (*sdkmcp.CallToolResult, BridgeOutput, error) {
	project, ref, err := h.options.Scope.IssueTargetIncludingArchived(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	action := strings.TrimSpace(input.Action)
	externalCommentID := strings.TrimSpace(input.ExternalCommentID)
	if action != "adopt" && action != "retry" && action != "skip" {
		return nil, BridgeOutput{}, errors.New("action must be adopt, retry, or skip")
	}
	if action == "adopt" && externalCommentID == "" {
		return nil, BridgeOutput{}, errors.New("adopt requires external_comment_id")
	}
	if action != "adopt" && externalCommentID != "" {
		return nil, BridgeOutput{}, errors.New("external_comment_id is only valid with adopt")
	}
	h = h.withLongRunningClient()
	response, err := h.options.Client.ResolveExternalComment(ctx, &generated.ResolveExternalCommentRequestOptions{
		PathParams: &generated.ResolveExternalCommentPath{ProjectID: project.ID, Ref: ref},
		Body: &generated.ResolveExternalCommentBody{
			Action: action, Actor: &h.options.Actor, ExternalCommentID: optionalString(externalCommentID),
		},
	})
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	return successResult(), BridgeOutput{Project: project, Bridge: *response}, nil
}

func (h toolHandlers) bridgeUnbind(ctx context.Context, _ *sdkmcp.CallToolRequest, input BridgeInput) (*sdkmcp.CallToolResult, BridgeOutput, error) {
	project, ref, err := h.options.Scope.IssueTargetIncludingArchived(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	response, err := h.options.Client.UnbindExternalRoot(ctx, &generated.UnbindExternalRootRequestOptions{
		PathParams: &generated.UnbindExternalRootPath{ProjectID: project.ID, Ref: ref},
		Query:      &generated.UnbindExternalRootQuery{Actor: &h.options.Actor},
	})
	if err != nil {
		return nil, BridgeOutput{}, err
	}
	return successResult(), BridgeOutput{Project: project, Bridge: *response}, nil
}

func (h toolHandlers) bridgeTarget(ctx context.Context, rawRef string, write bool) (ProjectIdentity, string, error) {
	return h.options.Scope.IssueTarget(ctx, h.options.Client, rawRef, write)
}

package mcpserver

import (
	"context"
	"errors"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// SyncStatusInput selects an issue synchronization binding.
type SyncStatusInput struct {
	Project  string `json:"project,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// SyncBindingSummary is a credential-free synchronization binding.
type SyncBindingSummary struct {
	ID              int64  `json:"id"`
	ProjectID       int64  `json:"project_id"`
	Provider        string `json:"provider"`
	SourceKey       string `json:"source_key"`
	RemoteID        string `json:"remote_id"`
	DisplayName     string `json:"display_name"`
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int64  `json:"interval_seconds"`
}

// SyncStatusOutput reports a synchronization binding and run state.
type SyncStatusOutput struct {
	Project ProjectIdentity              `json:"project"`
	Binding *SyncBindingSummary          `json:"binding,omitempty"`
	Status  generated.IssueSyncStatusOut `json:"status"`
}

// SyncUpdateInput enables or disables issue synchronization.
type SyncUpdateInput struct {
	Project         string         `json:"project,omitempty"`
	Provider        string         `json:"provider,omitempty"`
	Action          string         `json:"action"`
	Config          map[string]any `json:"config,omitempty"`
	Interval        string         `json:"interval,omitempty"`
	IntervalSeconds *int64         `json:"interval_seconds,omitempty"`
}

// SyncOnceInput selects a synchronization pass.
type SyncOnceInput struct {
	Project  string `json:"project,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// SyncOnceOutput reports one completed synchronization pass.
type SyncOnceOutput struct {
	Project ProjectIdentity              `json:"project"`
	Binding SyncBindingSummary           `json:"binding"`
	Import  generated.ImportBatchResult  `json:"import"`
	Status  generated.IssueSyncStatusOut `json:"status"`
}

func registerSyncTools(server *sdkmcp.Server, handlers toolHandlers) {
	addTool(server, "kata.sync_status", "Sync status", "Read issue synchronization state without returning credentials.", toolHints(true, false, true), handlers.syncStatus)
	addTool(server, "kata.sync_update", "Update sync", "Enable or disable issue synchronization for one project.", toolHints(false, true, true), handlers.syncUpdate)
	addTool(server, "kata.sync_once", "Run sync", "Run one issue synchronization pass; imported changes overwrite synced issue fields.", nonIdempotent(toolHints(false, true, true)), handlers.syncOnce)
}

func (h toolHandlers) syncStatus(ctx context.Context, _ *sdkmcp.CallToolRequest, input SyncStatusInput) (*sdkmcp.CallToolResult, SyncStatusOutput, error) {
	project, provider, err := h.syncTarget(ctx, input.Project, input.Provider)
	if err != nil {
		return nil, SyncStatusOutput{}, err
	}
	response, err := h.options.Client.GetIssueSyncStatus(ctx, &generated.GetIssueSyncStatusRequestOptions{PathParams: &generated.GetIssueSyncStatusPath{ProjectID: project.ID, Provider: provider}})
	if err != nil {
		return nil, SyncStatusOutput{}, err
	}
	return successResult(), syncStatusOutput(project, response), nil
}

func (h toolHandlers) syncUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, input SyncUpdateInput) (*sdkmcp.CallToolResult, SyncStatusOutput, error) {
	project, provider, err := h.syncTarget(ctx, input.Project, input.Provider)
	if err != nil {
		return nil, SyncStatusOutput{}, err
	}
	var response *generated.IssueSyncBody
	switch input.Action {
	case "enable":
		// Enabling sync selects which external repository the daemon's
		// globally configured credentials read, so it stays a daemon-wide
		// operator action. Scoped sessions may only disable or run the
		// operator-configured binding.
		if h.options.Scope.Mode() != ScopeAll {
			return nil, SyncStatusOutput{}, errors.New("enabling issue synchronization requires the --all-projects daemon-wide scope; scoped MCP can only disable or run the operator-configured binding")
		}
		response, err = h.options.Client.EnableIssueSync(ctx, &generated.EnableIssueSyncRequestOptions{PathParams: &generated.EnableIssueSyncPath{ProjectID: project.ID, Provider: provider}, Body: &generated.EnableIssueSyncBody{Config: input.Config, Interval: optionalString(input.Interval), IntervalSeconds: input.IntervalSeconds}})
	case "disable":
		empty := generated.DisableIssueSyncBody{}
		response, err = h.options.Client.DisableIssueSync(ctx, &generated.DisableIssueSyncRequestOptions{PathParams: &generated.DisableIssueSyncPath{ProjectID: project.ID, Provider: provider}, Body: &empty})
	default:
		return nil, SyncStatusOutput{}, errors.New("action must be enable or disable")
	}
	if err != nil {
		return nil, SyncStatusOutput{}, err
	}
	return successResult(), syncStatusOutput(project, response), nil
}

func (h toolHandlers) syncOnce(ctx context.Context, _ *sdkmcp.CallToolRequest, input SyncOnceInput) (*sdkmcp.CallToolResult, SyncOnceOutput, error) {
	project, provider, err := h.syncTarget(ctx, input.Project, input.Provider)
	if err != nil {
		return nil, SyncOnceOutput{}, err
	}
	empty := generated.RunIssueSyncOnceBody{}
	longRunning := h.withLongRunningClient()
	response, err := longRunning.options.Client.RunIssueSyncOnce(ctx, &generated.RunIssueSyncOnceRequestOptions{PathParams: &generated.RunIssueSyncOncePath{ProjectID: project.ID, Provider: provider}, Body: &empty})
	if err != nil {
		return nil, SyncOnceOutput{}, err
	}
	return successResult(), SyncOnceOutput{Project: project, Binding: syncBinding(response.Binding), Import: response.Import, Status: response.Status}, nil
}

func (h toolHandlers) syncTarget(ctx context.Context, projectName, provider string) (ProjectIdentity, string, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, projectName, false)
	if err != nil {
		return ProjectIdentity{}, "", err
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "github"
	}
	return project, provider, nil
}

func syncStatusOutput(project ProjectIdentity, body *generated.IssueSyncBody) SyncStatusOutput {
	output := SyncStatusOutput{Project: project, Status: body.Status}
	if body.Binding != nil {
		binding := syncBinding(*body.Binding)
		output.Binding = &binding
	}
	return output
}

func syncBinding(binding generated.IssueSyncBindingOut) SyncBindingSummary {
	return SyncBindingSummary{ID: binding.ID, ProjectID: binding.ProjectID, Provider: binding.Provider, SourceKey: binding.SourceKey, RemoteID: binding.RemoteID, DisplayName: binding.DisplayName, Enabled: binding.Enabled, IntervalSeconds: binding.IntervalSeconds}
}

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// ProjectsInput selects project records in the startup scope.
type ProjectsInput struct {
	Project         string `json:"project,omitempty"`
	IncludeArchived bool   `json:"include_archived,omitempty"`
}

// ProjectSummary is the compact MCP project representation.
type ProjectSummary struct {
	ID        int64          `json:"id"`
	UID       string         `json:"uid"`
	Name      string         `json:"name"`
	Revision  int64          `json:"revision"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Archived  bool           `json:"archived"`
	CreatedAt string         `json:"created_at"`
	DeletedAt string         `json:"deleted_at,omitempty"`
}

// ProjectsOutput contains project summaries.
type ProjectsOutput struct {
	Projects []ProjectSummary `json:"projects"`
}

// ProjectCreateInput names a path-free project to create.
type ProjectCreateInput struct {
	Name string `json:"name"`
}

// ProjectUpdateInput selects one supported project update action.
type ProjectUpdateInput struct {
	Project  string         `json:"project"`
	Action   string         `json:"action"`
	Name     string         `json:"name,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Revision *int64         `json:"revision,omitempty"`
	From     string         `json:"from,omitempty"`
	To       string         `json:"to,omitempty"`
	AliasID  int64          `json:"alias_id,omitempty"`
	Force    bool           `json:"force,omitempty"`
}

// ProjectMutationOutput reports a project mutation.
type ProjectMutationOutput struct {
	Project ProjectSummary `json:"project"`
	Changed bool           `json:"changed"`
	Event   *EventSummary  `json:"event,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// ProjectMergeInput selects source and target projects.
type ProjectMergeInput struct {
	Source     string `json:"source"`
	Target     string `json:"target"`
	TargetName string `json:"target_name,omitempty"`
}

// ProjectMergeOutput reports project merge counts and identities.
type ProjectMergeOutput struct {
	Source            ProjectSummary                    `json:"source"`
	Target            ProjectSummary                    `json:"target"`
	IssuesMoved       int64                             `json:"issues_moved"`
	EventsMoved       int64                             `json:"events_moved"`
	AliasesMoved      int64                             `json:"aliases_moved"`
	PurgeLogsMoved    int64                             `json:"purge_logs_moved"`
	ShortIDExtensions []generated.MergeShortIDExtension `json:"short_id_extensions,omitempty"`
}

// ProjectRemoveInput selects a project to archive.
type ProjectRemoveInput struct {
	Project string `json:"project"`
	Force   bool   `json:"force,omitempty"`
}

// ProjectRestoreInput selects an archived project to restore.
type ProjectRestoreInput struct {
	Project string `json:"project"`
}

// ProjectPurgeInput carries an irreversible project purge confirmation.
type ProjectPurgeInput struct {
	Project string `json:"project"`
	Confirm string `json:"confirm"`
	Reason  string `json:"reason,omitempty"`
}

// ProjectPurgeOutput reports a completed project purge.
type ProjectPurgeOutput struct {
	Project string                    `json:"project"`
	Purged  bool                      `json:"purged"`
	Log     generated.ProjectPurgeLog `json:"log"`
}

// SystemInput requests redacted daemon information.
type SystemInput struct{}

// SystemOutput contains redacted daemon identity and version information.
type SystemOutput struct {
	InstanceUID        string `json:"instance_uid"`
	Version            string `json:"version"`
	SchemaVersion      int64  `json:"schema_version"`
	AuthKind           string `json:"auth_kind"`
	AuthenticatedActor string `json:"authenticated_actor,omitempty"`
}

func registerProjectTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	mutating := toolHints(false, true, false)
	additive := toolHints(false, false, false)
	addTool(server, "kata.project_create", "Create project", "Create a path-free project in daemon-wide mode.", additive, handlers.projectCreate)
	addTool(server, "kata.project_merge", "Merge projects", "Merge one in-scope project into another.", mutating, handlers.projectMerge)
	addTool(server, "kata.project_purge", "Purge project", "Irreversibly purge an archived project after exact confirmation.", mutating, handlers.projectPurge)
	addTool(server, "kata.project_remove", "Archive project", "Archive an in-scope project.", mutating, handlers.projectRemove)
	addTool(server, "kata.project_restore", "Restore project", "Restore an archived in-scope project.", additive, handlers.projectRestore)
	addTool(server, "kata.project_update", "Update project", "Rename, patch metadata, rewrite authors, or detach an alias.", mutating, handlers.projectUpdate)
	addTool(server, "kata.projects", "List projects", "List or select projects inside the startup scope.", read, handlers.projects)
}

func registerSystemTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	addTool(server, "kata.system", "System information", "Read redacted daemon instance and version information.", read, handlers.system)
}

func (h toolHandlers) projects(ctx context.Context, _ *sdkmcp.CallToolRequest, input ProjectsInput) (*sdkmcp.CallToolResult, ProjectsOutput, error) {
	var include *string
	if input.IncludeArchived {
		value := "archived"
		include = &value
	}
	response, err := h.options.Client.ListProjects(ctx, &generated.ListProjectsRequestOptions{
		Query: &generated.ListProjectsQuery{Include: include},
	})
	if err != nil {
		return nil, ProjectsOutput{}, err
	}
	name := strings.TrimSpace(input.Project)
	result := make([]ProjectSummary, 0, len(response.Projects))
	for _, project := range response.Projects {
		if h.options.Scope.Mode() != ScopeAll && !h.projectInFixedScope(project) {
			continue
		}
		if name != "" && project.Name != name {
			continue
		}
		result = append(result, projectSummaryOut(project))
	}
	if name != "" && len(result) == 0 {
		for _, startup := range h.options.Scope.projects {
			if startup.Name == name {
				return nil, ProjectsOutput{}, fmt.Errorf("project %q in the MCP startup scope is no longer available", name)
			}
		}
		return nil, ProjectsOutput{}, fmt.Errorf("project %q is outside the MCP startup scope", name)
	}
	if h.options.Scope.Mode() != ScopeAll && name == "" && len(result) == 0 {
		return nil, ProjectsOutput{}, errors.New("no projects in the MCP startup scope are available")
	}
	return successResult(), ProjectsOutput{Projects: result}, nil
}

func (h toolHandlers) projectInFixedScope(project generated.ProjectOut) bool {
	for _, allowed := range h.options.Scope.projects {
		if allowed.UID != "" && allowed.UID == project.UID || allowed.UID == "" && allowed.ID == project.ID {
			return true
		}
	}
	return false
}

func (h toolHandlers) projectCreate(ctx context.Context, _ *sdkmcp.CallToolRequest, input ProjectCreateInput) (*sdkmcp.CallToolResult, ProjectMutationOutput, error) {
	if h.options.Scope.Mode() != ScopeAll {
		return nil, ProjectMutationOutput{}, errors.New("project creation requires daemon-wide scope")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, ProjectMutationOutput{}, errors.New("name is required")
	}
	response, err := h.options.Client.InitProject(ctx, &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Actor: &h.options.Actor, Name: &name}})
	if err != nil {
		return nil, ProjectMutationOutput{}, err
	}
	return successResult(), ProjectMutationOutput{Project: projectSummaryOut(response.Project), Changed: response.Created}, nil
}

func (h toolHandlers) projectUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, input ProjectUpdateInput) (*sdkmcp.CallToolResult, ProjectMutationOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, true)
	if err != nil {
		return nil, ProjectMutationOutput{}, err
	}
	switch input.Action {
	case "rename":
		name := strings.TrimSpace(input.Name)
		if name == "" {
			return nil, ProjectMutationOutput{}, errors.New("rename requires name")
		}
		response, renameErr := h.options.Client.RenameProject(ctx, &generated.RenameProjectRequestOptions{
			PathParams: &generated.RenameProjectPath{ProjectID: project.ID},
			Body:       &generated.RenameProjectBody{Actor: &h.options.Actor, Name: name},
		})
		if renameErr != nil {
			return nil, ProjectMutationOutput{}, renameErr
		}
		return successResult(), ProjectMutationOutput{Project: projectSummaryOut(response.Project), Changed: true}, nil
	case "metadata":
		if len(input.Metadata) == 0 {
			return nil, ProjectMutationOutput{}, errors.New("metadata action requires metadata")
		}
		var headers *generated.PatchProjectMetadataHeaders
		if input.Revision != nil {
			ifMatch := `"rev-` + strconv.FormatInt(*input.Revision, 10) + `"`
			headers = &generated.PatchProjectMetadataHeaders{IfMatch: &ifMatch}
		}
		response, patchErr := h.options.Client.PatchProjectMetadata(ctx, &generated.PatchProjectMetadataRequestOptions{
			PathParams: &generated.PatchProjectMetadataPath{ProjectID: project.ID},
			Body:       &generated.PatchProjectMetadataBody{Actor: h.options.Actor, Patch: input.Metadata}, Header: headers,
		})
		if patchErr != nil {
			return nil, ProjectMutationOutput{}, patchErr
		}
		return successResult(), ProjectMutationOutput{Project: projectSummary(response.Project), Changed: response.Changed, Event: eventSummary(response.Event)}, nil
	case "rewrite_author":
		if strings.TrimSpace(input.From) == "" || strings.TrimSpace(input.To) == "" {
			return nil, ProjectMutationOutput{}, errors.New("rewrite_author requires from and to")
		}
		response, rewriteErr := h.options.Client.RewriteAuthorIdentity(ctx, &generated.RewriteAuthorIdentityRequestOptions{
			PathParams: &generated.RewriteAuthorIdentityPath{ProjectID: project.ID},
			Body:       &generated.RewriteAuthorIdentityBody{Actor: &h.options.Actor, From: input.From, To: input.To},
		})
		if rewriteErr != nil {
			return nil, ProjectMutationOutput{}, rewriteErr
		}
		return successResult(), ProjectMutationOutput{Project: ProjectSummary{ID: project.ID, UID: project.UID, Name: project.Name}, Changed: response.Changed, Event: eventSummary(response.Event), Details: map[string]any{"total": response.Total, "issue_authors": response.IssueAuthors, "issue_owners": response.IssueOwners, "comment_authors": response.CommentAuthors, "link_authors": response.LinkAuthors}}, nil
	case "detach_alias":
		if input.AliasID <= 0 {
			return nil, ProjectMutationOutput{}, errors.New("detach_alias requires alias_id")
		}
		response, detachErr := h.options.Client.DetachProjectAlias(ctx, &generated.DetachProjectAliasRequestOptions{
			PathParams: &generated.DetachProjectAliasPath{ProjectID: project.ID, AliasID: input.AliasID},
			Query:      &generated.DetachProjectAliasQuery{Actor: h.options.Actor, Force: optionalTrue(input.Force)},
		})
		if detachErr != nil {
			return nil, ProjectMutationOutput{}, detachErr
		}
		return successResult(), ProjectMutationOutput{Project: ProjectSummary{ID: project.ID, UID: project.UID, Name: project.Name}, Changed: true, Event: eventSummary(&response.Event), Details: map[string]any{"alias_id": response.Alias.ID, "alias_kind": response.Alias.AliasKind}}, nil
	default:
		return nil, ProjectMutationOutput{}, errors.New("action must be rename, metadata, rewrite_author, or detach_alias")
	}
}

func (h toolHandlers) projectMerge(ctx context.Context, _ *sdkmcp.CallToolRequest, input ProjectMergeInput) (*sdkmcp.CallToolResult, ProjectMergeOutput, error) {
	source, err := h.options.Scope.Project(ctx, h.options.Client, input.Source, false)
	if err != nil {
		return nil, ProjectMergeOutput{}, err
	}
	target, err := h.options.Scope.Project(ctx, h.options.Client, input.Target, false)
	if err != nil {
		return nil, ProjectMergeOutput{}, err
	}
	response, err := h.options.Client.MergeProject(ctx, &generated.MergeProjectRequestOptions{
		PathParams: &generated.MergeProjectPath{ProjectID: target.ID},
		Body:       &generated.MergeProjectBody{Actor: &h.options.Actor, SourceProjectID: source.ID, TargetName: optionalString(input.TargetName)},
	})
	if err != nil {
		return nil, ProjectMergeOutput{}, err
	}
	return successResult(), ProjectMergeOutput{Source: projectSummaryOut(response.Source), Target: projectSummaryOut(response.Target), IssuesMoved: response.IssuesMoved, EventsMoved: response.EventsMoved, AliasesMoved: response.AliasesMoved, PurgeLogsMoved: response.PurgeLogsMoved, ShortIDExtensions: response.ShortIDExtensions}, nil
}

func (h toolHandlers) projectRemove(ctx context.Context, _ *sdkmcp.CallToolRequest, input ProjectRemoveInput) (*sdkmcp.CallToolResult, ProjectMutationOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, ProjectMutationOutput{}, err
	}
	response, err := h.options.Client.RemoveProject(ctx, &generated.RemoveProjectRequestOptions{
		PathParams: &generated.RemoveProjectPath{ProjectID: project.ID},
		Query:      &generated.RemoveProjectQuery{Actor: h.options.Actor, Force: optionalTrue(input.Force)},
	})
	if err != nil {
		return nil, ProjectMutationOutput{}, err
	}
	return successResult(), ProjectMutationOutput{Project: projectSummaryOut(response.Project), Changed: true, Event: eventSummary(&response.Event)}, nil
}

func (h toolHandlers) projectRestore(ctx context.Context, _ *sdkmcp.CallToolRequest, input ProjectRestoreInput) (*sdkmcp.CallToolResult, ProjectMutationOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, true)
	if err != nil {
		return nil, ProjectMutationOutput{}, err
	}
	response, err := h.options.Client.RestoreProject(ctx, &generated.RestoreProjectRequestOptions{
		PathParams: &generated.RestoreProjectPath{ProjectID: project.ID},
		Query:      &generated.RestoreProjectQuery{Actor: h.options.Actor},
	})
	if err != nil {
		return nil, ProjectMutationOutput{}, err
	}
	return successResult(), ProjectMutationOutput{Project: projectSummaryOut(response.Project), Changed: response.Changed, Event: eventSummary(&response.Event)}, nil
}

func (h toolHandlers) projectPurge(ctx context.Context, _ *sdkmcp.CallToolRequest, input ProjectPurgeInput) (*sdkmcp.CallToolResult, ProjectPurgeOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, true)
	if err != nil {
		return nil, ProjectPurgeOutput{}, err
	}
	expected := "PURGE " + project.Name
	if input.Confirm != expected {
		return nil, ProjectPurgeOutput{}, fmt.Errorf("confirm must equal %q", expected)
	}
	response, err := h.options.Client.PurgeProject(ctx, &generated.PurgeProjectRequestOptions{
		PathParams: &generated.PurgeProjectPath{ProjectID: project.ID},
		Body:       &generated.PurgeProjectBody{Actor: h.options.Actor, Reason: optionalString(input.Reason)},
		Header:     &generated.PurgeProjectHeaders{XKataConfirm: &expected},
	})
	if err != nil {
		return nil, ProjectPurgeOutput{}, err
	}
	return successResult(), ProjectPurgeOutput{Project: project.Name, Purged: true, Log: response.ProjectPurgeLog}, nil
}

func (h toolHandlers) system(ctx context.Context, _ *sdkmcp.CallToolRequest, _ SystemInput) (*sdkmcp.CallToolResult, SystemOutput, error) {
	response, err := h.options.Client.Instance(ctx)
	if err != nil {
		return nil, SystemOutput{}, err
	}
	actor := ""
	if response.Auth.Actor != nil {
		actor = *response.Auth.Actor
	}
	return successResult(), SystemOutput{InstanceUID: response.InstanceUID, Version: response.Version, SchemaVersion: response.SchemaVersion, AuthKind: response.Auth.Kind, AuthenticatedActor: actor}, nil
}

func projectSummaryOut(project generated.ProjectOut) ProjectSummary {
	result := ProjectSummary{ID: project.ID, UID: project.UID, Name: project.Name, Revision: project.Revision, Metadata: project.Metadata, CreatedAt: formatTime(project.CreatedAt), Archived: project.DeletedAt != nil}
	if project.DeletedAt != nil {
		result.DeletedAt = formatTime(*project.DeletedAt)
	}
	return result
}

func projectSummary(project generated.Project) ProjectSummary {
	result := ProjectSummary{ID: project.ID, UID: project.UID, Name: project.Name, Revision: project.Revision, Metadata: project.Metadata, CreatedAt: formatTime(project.CreatedAt), Archived: project.DeletedAt != nil}
	if project.DeletedAt != nil {
		result.DeletedAt = formatTime(*project.DeletedAt)
	}
	return result
}

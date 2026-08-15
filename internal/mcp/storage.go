package mcpserver

import (
	"context"
	"errors"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/storageadmin"
)

// StorageExportInput selects a root-contained JSONL artifact.
type StorageExportInput struct {
	Artifact       string `json:"artifact"`
	Project        string `json:"project,omitempty"`
	IncludeDeleted *bool  `json:"include_deleted,omitempty"`
	Force          bool   `json:"force,omitempty"`
	Confirm        string `json:"confirm,omitempty"`
}

// StorageExportOutput reports an installed JSONL artifact.
type StorageExportOutput struct {
	Artifact string `json:"artifact"`
	Bytes    int64  `json:"bytes"`
	Project  string `json:"project,omitempty"`
}

// StorageImportInput selects an artifact and configured target alias.
type StorageImportInput struct {
	Artifact    string `json:"artifact"`
	Target      string `json:"target"`
	Force       bool   `json:"force,omitempty"`
	Confirm     string `json:"confirm,omitempty"`
	NewInstance bool   `json:"new_instance,omitempty"`
}

// StorageImportOutput reports a completed storage restore.
type StorageImportOutput struct {
	Artifact string `json:"artifact"`
	Target   string `json:"target"`
	Backend  string `json:"backend"`
}

func registerStorageTools(server *sdkmcp.Server, handlers toolHandlers) {
	if handlers.options.StorageAdmin == nil {
		return
	}
	addTool(server, "kata.storage_export", "Export storage", "Export Kata JSONL to an artifact under the operator-approved storage root; force overwrites an existing artifact.", toolHints(false, true, false), handlers.storageExport)
	addTool(server, "kata.storage_import", "Import storage", "Restore Kata JSONL to a startup-configured storage target alias; a forced retry replaces the target again, and new_instance mints a fresh instance identity each run.", nonIdempotent(toolHints(false, true, false)), handlers.storageImport)
}

func (h toolHandlers) storageExport(ctx context.Context, _ *sdkmcp.CallToolRequest, input StorageExportInput) (*sdkmcp.CallToolResult, StorageExportOutput, error) {
	if h.options.StorageAdmin == nil {
		return nil, StorageExportOutput{}, errors.New("storage administration is not enabled")
	}
	// A project-filtered JSONL export still contains cross-project link
	// rows and unredacted event payload references, so exporting is a
	// daemon-wide operator action regardless of the project filter.
	if err := h.requireAllProjectsScope("storage export"); err != nil {
		return nil, StorageExportOutput{}, err
	}
	projectID := int64(0)
	projectName := ""
	if strings.TrimSpace(input.Project) != "" {
		project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
		if err != nil {
			return nil, StorageExportOutput{}, err
		}
		projectID, projectName = project.ID, project.Name
	}
	result, err := h.options.StorageAdmin.Export(ctx, storageadmin.ExportOptions{
		Artifact: input.Artifact, ProjectID: projectID, IncludeDeleted: input.includeDeleted(),
		Force: input.Force, Confirm: input.Confirm,
	})
	if err != nil {
		return nil, StorageExportOutput{}, err
	}
	return successResult(), StorageExportOutput{Artifact: result.Artifact, Bytes: result.Bytes, Project: projectName}, nil
}

func (input StorageExportInput) includeDeleted() bool {
	return input.IncludeDeleted == nil || *input.IncludeDeleted
}

func (h toolHandlers) storageImport(ctx context.Context, _ *sdkmcp.CallToolRequest, input StorageImportInput) (*sdkmcp.CallToolResult, StorageImportOutput, error) {
	if h.options.StorageAdmin == nil {
		return nil, StorageImportOutput{}, errors.New("storage administration is not enabled")
	}
	result, err := h.options.StorageAdmin.Import(ctx, storageadmin.ImportOptions{Artifact: input.Artifact, Target: input.Target, Force: input.Force, Confirm: input.Confirm, NewInstance: input.NewInstance})
	if err != nil {
		return nil, StorageImportOutput{}, err
	}
	return successResult(), StorageImportOutput{Artifact: result.Artifact, Target: result.Target, Backend: result.Backend}, nil
}

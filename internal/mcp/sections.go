package mcpserver

import (
	"context"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSectionInput loads one section's typed tools into the MCP catalog.
type ToolSectionInput struct{}

// ToolSectionOutput reports the tools available through one section.
type ToolSectionOutput struct {
	Section   string   `json:"section"`
	Available bool     `json:"available"`
	Loaded    bool     `json:"loaded"`
	Tools     []string `json:"tools"`
}

type toolSection struct {
	loader      string
	section     string
	title       string
	description string
	tools       []string
	available   bool
	register    func(*sdkmcp.Server, toolHandlers)
}

func registerSectionLoaders(server *sdkmcp.Server, options Options) {
	handlers := toolHandlers{options: options}
	sections := toolSections(
		options.StorageAdmin != nil,
		options.EnableTokenAdmin && options.Scope.Mode() == ScopeAll,
	)
	loaded := make(map[string]bool, len(sections))
	var mutex sync.Mutex
	for _, definition := range sections {
		definition := definition
		addTool(server, definition.loader, definition.title, definition.description,
			toolHints(true, false, false),
			func(_ context.Context, _ *sdkmcp.CallToolRequest, _ ToolSectionInput) (*sdkmcp.CallToolResult, ToolSectionOutput, error) {
				mutex.Lock()
				defer mutex.Unlock()
				if definition.available && !loaded[definition.section] {
					definition.register(server, handlers)
					loaded[definition.section] = true
				}
				return successResult(), ToolSectionOutput{
					Section: definition.section, Available: definition.available,
					Loaded: loaded[definition.section], Tools: append([]string(nil), definition.tools...),
				}, nil
			})
	}
}

func toolSections(storageAvailable, tokenAdminAvailable bool) []toolSection {
	return []toolSection{
		{
			loader: "kata.load_activity", section: "activity", title: "Load activity tools",
			description: "Load typed digest and event-stream tools, then refresh the MCP tool list.",
			tools:       []string{"kata.digest", "kata.events"}, available: true, register: registerActivityTools,
		},
		{
			loader: "kata.load_federation", section: "federation", title: "Load federation tools",
			description: "Load typed federation enrollment, replica, leave, and quarantine tools, then refresh the MCP tool list.",
			tools:       []string{"kata.federation_enroll", "kata.federation_enrollment_revoke", "kata.federation_join", "kata.federation_leave", "kata.federation_quarantine", "kata.federation_rebind", "kata.federation_status"},
			available:   true, register: registerFederationTools,
		},
		{
			loader: "kata.load_import", section: "import", title: "Load import tools",
			description: "Load the typed structured issue-import tool, then refresh the MCP tool list.",
			tools:       []string{"kata.import_issues"}, available: true, register: registerImportTools,
		},
		{
			loader: "kata.load_issue_discovery", section: "issue_discovery", title: "Load issue discovery tools",
			description: "Load typed issue search, list, show, ready, next, label, and graph tools, then refresh the MCP tool list.",
			tools:       []string{"kata.graph", "kata.labels", "kata.list", "kata.next", "kata.ready", "kata.search", "kata.show"},
			available:   true, register: registerIssueDiscoveryTools,
		},
		{
			loader: "kata.load_issue_lifecycle", section: "issue_lifecycle", title: "Load issue lifecycle tools",
			description: "Load typed close, reopen, delete, restore, purge, wait, and close-audit tools, then refresh the MCP tool list.",
			tools:       []string{"kata.audit_closes", "kata.close", "kata.delete", "kata.purge", "kata.reopen", "kata.restore", "kata.wait"},
			available:   true, register: registerIssueLifecycleTools,
		},
		{
			loader: "kata.load_issue_mutation", section: "issue_mutation", title: "Load issue mutation tools",
			description: "Load typed issue creation, edit, comment, claim, label, metadata, planning-date, and move tools, then refresh the MCP tool list.",
			tools:       []string{"kata.claim", "kata.comment", "kata.create", "kata.edit", "kata.edit_comment", "kata.move", "kata.set_deadline", "kata.set_label", "kata.set_metadata", "kata.set_schedule"},
			available:   true, register: registerIssueMutationTools,
		},
		{
			loader: "kata.load_leases", section: "leases", title: "Load lease tools",
			description: "Load typed federation lease status and mutation tools, then refresh the MCP tool list.",
			tools:       []string{"kata.lease", "kata.lease_force_release", "kata.lease_status", "kata.lease_steal"},
			available:   true, register: registerLeaseTools,
		},
		{
			loader: "kata.load_projects", section: "projects", title: "Load project tools",
			description: "Load typed project discovery and administration tools, then refresh the MCP tool list.",
			tools:       []string{"kata.project_create", "kata.project_merge", "kata.project_purge", "kata.project_remove", "kata.project_restore", "kata.project_update", "kata.projects"},
			available:   true, register: registerProjectTools,
		},
		{
			loader: "kata.load_recurrence", section: "recurrence", title: "Load recurrence tools",
			description: "Load typed recurrence discovery and administration tools, then refresh the MCP tool list.",
			tools:       []string{"kata.recurrence_delete", "kata.recurrence_update", "kata.recurrences"},
			available:   true, register: registerRecurrenceTools,
		},
		{
			loader: "kata.load_storage", section: "storage", title: "Load storage tools",
			description: "Load typed JSONL storage tools when host storage was enabled at startup, then refresh the MCP tool list.",
			tools:       []string{"kata.storage_export", "kata.storage_import"}, available: storageAvailable, register: registerStorageTools,
		},
		{
			loader: "kata.load_sync", section: "sync", title: "Load synchronization tools",
			description: "Load typed GitHub synchronization tools, then refresh the MCP tool list.",
			tools:       []string{"kata.sync_once", "kata.sync_status", "kata.sync_update"}, available: true, register: registerSyncTools,
		},
		{
			loader: "kata.load_system", section: "system", title: "Load system tools",
			description: "Load the typed redacted daemon system-information tool, then refresh the MCP tool list.",
			tools:       []string{"kata.system"}, available: true, register: registerSystemTools,
		},
		{
			loader: "kata.load_tokens", section: "tokens", title: "Load token tools",
			description: "Load typed token administration tools when explicitly enabled at startup, then refresh the MCP tool list.",
			tools:       []string{"kata.token_create", "kata.token_revoke", "kata.tokens"}, available: tokenAdminAvailable, register: registerTokenTools,
		},
	}
}

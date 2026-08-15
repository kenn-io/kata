// Package mcpserver exposes Kata issue operations through MCP.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"go.kenn.io/kata/internal/storageadmin"
	kataclient "go.kenn.io/kata/pkg/client"
)

const catalogTTL = 5 * time.Minute

const (
	toolCallsPerSecond  = 20
	toolCallBurst       = 20
	toolCallConcurrency = 8
)

// Options fixes the Kata daemon scope and actor for one MCP process.
type Options struct {
	Client            *kataclient.Client
	LongRunningClient *kataclient.Client
	Scope             *Scope
	ProjectID         int64
	ProjectName       string
	Actor             string
	Version           string
	StorageAdmin      *storageadmin.Admin
	EnableTokenAdmin  bool
}

// New creates a tools-only MCP server for one startup project scope.
func New(options Options) (*sdkmcp.Server, error) {
	if options.Client == nil {
		return nil, errors.New("kata API client is required")
	}
	if options.LongRunningClient == nil {
		options.LongRunningClient = options.Client
	}
	if options.Scope == nil {
		scope, err := NewBoundScope(ProjectIdentity{ID: options.ProjectID, Name: options.ProjectName})
		if err != nil {
			return nil, err
		}
		options.Scope = scope
	}
	if options.Scope.Mode() == ScopeBound {
		projects, err := options.Scope.Projects(context.Background(), nil, false)
		if err != nil {
			return nil, err
		}
		options.ProjectID = projects[0].ID
		options.ProjectName = projects[0].Name
	}
	if strings.TrimSpace(options.Actor) == "" {
		return nil, errors.New("kata actor is required")
	}
	if strings.TrimSpace(options.Version) == "" {
		return nil, errors.New("kata server version is required")
	}

	scopeDescription := "startup project scope"
	if options.Scope.Mode() == ScopeBound {
		scopeDescription = "project " + options.ProjectName
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:        "kata",
		Title:       "Kata issue tracker",
		Description: "Scoped Kata data and administration tools for coding agents.",
		Version:     options.Version,
	}, &sdkmcp.ServerOptions{
		Instructions: "Use Kata to search before creating work, claim actionable issues, record progress, and close only with evidence. All tools are fixed to " + scopeDescription + " and actor " + options.Actor + ".",
		Capabilities: &sdkmcp.ServerCapabilities{
			Tools: &sdkmcp.ToolCapabilities{ListChanged: true},
		},
	})
	server.AddReceivingMiddleware(toolAdmissionMiddleware(
		rate.NewLimiter(toolCallsPerSecond, toolCallBurst),
		make(chan struct{}, toolCallConcurrency),
	))
	server.AddReceivingMiddleware(cacheHintsMiddleware)
	registerSectionLoaders(server, options)
	return server, nil
}

func toolAdmissionMiddleware(limiter *rate.Limiter, concurrent chan struct{}) sdkmcp.Middleware {
	return func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, request)
			}
			select {
			case concurrent <- struct{}{}:
				defer func() { <-concurrent }()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if err := limiter.Wait(ctx); err != nil {
				return nil, err
			}
			return next(ctx, method, request)
		}
	}
}

func cacheHintsMiddleware(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
		result, err := next(ctx, method, request)
		if err != nil {
			return nil, err
		}
		switch result := result.(type) {
		case *sdkmcp.DiscoverResult:
			result.TTLMs = int(catalogTTL.Milliseconds())
			result.CacheScope = "private"
		case *sdkmcp.ListToolsResult:
			result.TTLMs = int(catalogTTL.Milliseconds())
			result.CacheScope = "private"
		}
		return result, nil
	}
}

func registerIssueDiscoveryTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	searchRead := toolHints(true, false, true)
	addTool(server, "kata.graph", "Issue graph", "Read the bounded reachable relationship graph for one issue.", read, handlers.graph)
	addTool(server, "kata.labels", "List labels", "List labels already used in the selected project scope with issue counts.", read, handlers.labels)
	addTool(server, "kata.list", "List issues", "List compact issue summaries in the selected project scope with bounded filters.", read, handlers.list)
	addTool(server, "kata.next", "Next issue", "Select the highest-priority ready issue.", read, handlers.next)
	addTool(server, "kata.ready", "List ready issues", "List open issues with no open blocking predecessor in the selected project scope.", read, handlers.ready)
	addTool(server, "kata.search", "Search issues", "Search titles, bodies, and comments before creating duplicate work.", searchRead, handlers.search)
	addTool(server, "kata.show", "Show issue", "Read one issue with its body, metadata, relationships, and a bounded comment history.", read, handlers.show)
}

func registerIssueMutationTools(server *sdkmcp.Server, handlers toolHandlers) {
	additive := toolHints(false, false, false)
	mutating := toolHints(false, true, false)
	addTool(server, "kata.claim", "Claim issue", "Claim an issue for the bound actor. Without force the call fails when another owner already holds it; force replaces that owner.", mutating, handlers.claim)
	addTool(server, "kata.comment", "Add comment", "Append a progress or context comment using a required idempotency key.", additive, handlers.comment)
	addTool(server, "kata.create", "Create issue", "Create project work after searching for duplicates. A stable idempotency key is required.", additive, handlers.create)
	addTool(server, "kata.edit", "Edit issue", "Edit issue fields, ownership, priority, and relationship deltas atomically.", mutating, handlers.edit)
	addTool(server, "kata.edit_comment", "Edit comment", "Replace one comment body as the startup actor.", mutating, handlers.editComment)
	addTool(server, "kata.move", "Move issue", "Move an issue to another in-scope project while preserving its UID and relationships.", mutating, handlers.move)
	addTool(server, "kata.set_deadline", "Set deadline", "Set or clear an issue deadline. Values accept a date, local date-time, or RFC 3339 UTC instant ending in Z.", mutating, handlers.setDeadline)
	addTool(server, "kata.set_label", "Set label presence", "Ensure that a label is present or absent on an issue.", mutating, handlers.setLabel)
	addTool(server, "kata.set_metadata", "Patch metadata", "Patch issue metadata; JSON null removes a key. An optional revision makes the write conditional.", mutating, handlers.setMetadata)
	addTool(server, "kata.set_schedule", "Set schedule", "Set or clear an issue schedule gate. Values accept a date, local date-time, or RFC 3339 UTC instant ending in Z.", mutating, handlers.setSchedule)
}

func registerIssueLifecycleTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	additive := toolHints(false, false, false)
	mutating := toolHints(false, true, false)
	addTool(server, "kata.audit_closes", "Audit closes", "List close records with bounded project and time filters.", read, handlers.auditCloses)
	addTool(server, "kata.close", "Close issue", "Close a completed issue with a reason, substantive message, and typed evidence.", mutating, handlers.close)
	addTool(server, "kata.delete", "Delete issue", "Soft-delete an issue after exact confirmation.", mutating, handlers.deleteIssue)
	addTool(server, "kata.purge", "Purge issue", "Irreversibly purge a soft-deleted issue after exact confirmation.", mutating, handlers.purgeIssue)
	addTool(server, "kata.reopen", "Reopen issue", "Reopen a closed issue when new work is required.", mutating, handlers.reopen)
	addTool(server, "kata.restore", "Restore issue", "Restore a soft-deleted issue.", additive, handlers.restoreIssue)
	addTool(server, "kata.wait", "Wait for issues", "Wait for issue status conditions with a bounded timeout.", read, handlers.wait)
}

func addTool[In, Out any](
	server *sdkmcp.Server,
	name string,
	title string,
	description string,
	annotations *sdkmcp.ToolAnnotations,
	handler sdkmcp.ToolHandlerFor[In, Out],
) {
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:         name,
		Title:        title,
		Description:  description,
		Annotations:  annotations,
		InputSchema:  inputSchemaFor[In](name),
		OutputSchema: schemaFor[Out](),
	}, handler)
}

func schemaFor[T any]() *jsonschema.Schema {
	schema, err := jsonschema.ForType(reflect.TypeFor[T](), &jsonschema.ForOptions{})
	if err != nil {
		panic(fmt.Errorf("infer MCP schema for %T: %w", *new(T), err))
	}
	schema.Schema = "https://json-schema.org/draft/2020-12/schema"
	return schema
}

func inputSchemaFor[T any](toolName string) *jsonschema.Schema {
	schema := schemaFor[T]()
	property := func(name string) *jsonschema.Schema {
		return schema.Properties[name]
	}
	setStringBounds := func(name string, minimum, maximum int) {
		if field := property(name); field != nil {
			field.MinLength = &minimum
			if maximum > 0 {
				field.MaxLength = &maximum
			}
		}
	}
	setNumberBounds := func(name string, minimum, maximum float64) {
		if field := property(name); field != nil {
			field.Minimum = &minimum
			field.Maximum = &maximum
		}
	}
	setEnum := func(name string, values ...any) {
		if field := property(name); field != nil {
			field.Enum = values
		}
	}
	forbidTogether := func(names ...string) {
		schema.AllOf = append(schema.AllOf, &jsonschema.Schema{
			Not: &jsonschema.Schema{Required: names},
		})
	}
	forbidTrueWith := func(booleanName, otherName string) {
		trueValue := any(true)
		schema.AllOf = append(schema.AllOf, &jsonschema.Schema{
			Not: &jsonschema.Schema{
				Required: []string{booleanName, otherName},
				Properties: map[string]*jsonschema.Schema{
					booleanName: {Const: &trueValue},
				},
			},
		})
	}

	switch toolName {
	case "kata.search":
		setStringBounds("query", 1, 4096)
		setNumberBounds("limit", 1, maximumResultLimit)
		setEnum("mode", "auto", "lexical", "hybrid", "semantic")
	case "kata.list":
		setNumberBounds("limit", 1, maximumResultLimit)
		setNumberBounds("priority", 0, 4)
		setNumberBounds("max_priority", 0, 4)
		setEnum("status", "open", "closed")
		forbidTrueWith("unowned", "owner")
	case "kata.ready":
		setNumberBounds("limit", 1, maximumResultLimit)
		forbidTrueWith("unowned", "owner")
	case "kata.show":
		setStringBounds("ref", 1, 256)
		setNumberBounds("comment_limit", 1, maximumResultLimit)
	case "kata.create":
		setStringBounds("title", 1, 1024)
		setStringBounds("idempotency_key", 1, 256)
		setNumberBounds("priority", 0, 4)
	case "kata.edit":
		setStringBounds("ref", 1, 256)
		setNumberBounds("priority", 0, 4)
		forbidTrueWith("clear_owner", "owner")
		forbidTrueWith("clear_priority", "priority")
		forbidTrueWith("clear_scheduled_on", "scheduled_on")
		forbidTrueWith("clear_timezone", "timezone")
		forbidTogether("parent", "remove_parent")
	case "kata.comment":
		setStringBounds("ref", 1, 256)
		setStringBounds("body", 1, 1<<20)
		setStringBounds("idempotency_key", 1, 256)
	case "kata.claim":
		setStringBounds("ref", 1, 256)
	case "kata.set_label":
		setStringBounds("ref", 1, 256)
		setStringBounds("label", 1, 64)
		property("label").Pattern = `^[a-z0-9._:-]+$`
	case "kata.set_deadline":
		setStringBounds("ref", 1, 256)
		setStringBounds("deadline", 1, 64)
		if field := property("revision"); field != nil {
			minimum := float64(0)
			field.Minimum = &minimum
		}
		schema.OneOf = planningDateSchemas("deadline", "clear_deadline")
	case "kata.set_metadata":
		setStringBounds("ref", 1, 256)
		if field := property("revision"); field != nil {
			minimum := float64(0)
			field.Minimum = &minimum
		}
		if field := property("patch"); field != nil {
			minimum := 1
			field.MinProperties = &minimum
		}
	case "kata.set_schedule":
		setStringBounds("ref", 1, 256)
		setStringBounds("schedule", 1, 64)
		if field := property("revision"); field != nil {
			minimum := float64(0)
			field.Minimum = &minimum
		}
		schema.OneOf = planningDateSchemas("schedule", "clear_schedule")
	case "kata.recurrence_update":
		setEnum("action", "create", "patch")
		if field := property("revision"); field != nil {
			minimum := float64(1)
			field.Minimum = &minimum
		}
		patchAction := any("patch")
		schema.AllOf = append(schema.AllOf, &jsonschema.Schema{
			If: &jsonschema.Schema{
				Required: []string{"action"},
				Properties: map[string]*jsonschema.Schema{
					"action": {Const: &patchAction},
				},
			},
			Then: &jsonschema.Schema{Required: []string{"uid", "revision"}},
		})
	case "kata.federation_leave":
		setEnum("phase", "preflight", "prepare", "commit")
		setEnum("disposition", "detach", "archive")
		commit := any("commit")
		schema.AllOf = append(schema.AllOf, &jsonschema.Schema{
			If: &jsonschema.Schema{
				Required: []string{"phase"},
				Properties: map[string]*jsonschema.Schema{
					"phase": {Const: &commit},
				},
			},
			Then: &jsonschema.Schema{Required: []string{"confirm"}},
		})
	case "kata.close":
		setStringBounds("ref", 1, 256)
		setStringBounds("message", 1, 1<<20)
		setEnum("reason", "done", "wontfix", "duplicate", "superseded", "audit-no-change")
		property("evidence").Items = evidenceSchema()
		schema.OneOf = closeReasonSchemas()
	case "kata.reopen":
		setStringBounds("ref", 1, 256)
	case "kata.audit_closes":
		setNumberBounds("limit", 1, maximumResultLimit)
		setStringBounds("cursor", 1, 64)
	}
	return schema
}

func planningDateSchemas(valueField, clearField string) []*jsonschema.Schema {
	trueValue := any(true)
	return []*jsonschema.Schema{
		{
			Required: []string{valueField},
			Not:      &jsonschema.Schema{Required: []string{clearField}},
		},
		{
			Required: []string{clearField},
			Properties: map[string]*jsonschema.Schema{
				clearField: {Const: &trueValue},
			},
			Not: &jsonschema.Schema{Required: []string{valueField}},
		},
	}
}

func evidenceVariant(kind string) *jsonschema.Schema {
	stringValue := func(minimum, maximum int) *jsonschema.Schema {
		return &jsonschema.Schema{Type: "string", MinLength: &minimum, MaxLength: &maximum}
	}
	variant := func(kind, field string, fieldSchema *jsonschema.Schema) *jsonschema.Schema {
		kindValue := any(kind)
		return &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"type": {Type: "string", Const: &kindValue},
				field:  fieldSchema,
			},
			Required:             []string{"type", field},
			AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		}
	}
	pathMinimum := 1
	paths := &jsonschema.Schema{
		Type:     "array",
		Items:    stringValue(1, 4096),
		MinItems: &pathMinimum,
	}
	switch kind {
	case "commit":
		sha := stringValue(7, 40)
		sha.Pattern = `^[0-9a-fA-F]{7,40}$`
		return variant(kind, "sha", sha)
	case "pr":
		return variant(kind, "url", stringValue(1, 4096))
	case "test":
		return variant(kind, "command", stringValue(1, 1<<20))
	case "reviewed-paths":
		return variant(kind, "paths", paths)
	case "no-change-audit":
		return variant(kind, "rationale", stringValue(1, 1<<20))
	case "duplicate-of", "superseded-by":
		return variant(kind, "issue_ref", stringValue(1, 256))
	default:
		panic("unknown evidence kind " + kind)
	}
}

func evidenceSchemaFor(kinds ...string) *jsonschema.Schema {
	variants := make([]*jsonschema.Schema, 0, len(kinds))
	for _, kind := range kinds {
		variants = append(variants, evidenceVariant(kind))
	}
	return &jsonschema.Schema{OneOf: variants}
}

func evidenceSchema() *jsonschema.Schema {
	return evidenceSchemaFor("commit", "pr", "test", "reviewed-paths", "no-change-audit", "duplicate-of", "superseded-by")
}

func closeReasonSchemas() []*jsonschema.Schema {
	variant := func(reason string, messageMinimum, evidenceMinimum, evidenceMaximum int, kinds ...string) *jsonschema.Schema {
		reasonValue := any(reason)
		messageMaximum := 1 << 20
		evidence := &jsonschema.Schema{
			Type:     "array",
			Items:    evidenceSchemaFor(kinds...),
			MinItems: &evidenceMinimum,
		}
		if evidenceMaximum >= 0 {
			evidence.MaxItems = &evidenceMaximum
		}
		return &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"reason":   {Type: "string", Const: &reasonValue},
				"message":  {Type: "string", MinLength: &messageMinimum, MaxLength: &messageMaximum},
				"evidence": evidence,
			},
			Required: []string{"reason", "message", "evidence"},
		}
	}
	done := variant("done", 40, 1, -1, "commit", "pr", "test", "reviewed-paths")
	wontfix := variant("wontfix", 60, 0, 0, "commit")
	duplicate := variant("duplicate", 20, 1, 1, "duplicate-of")
	superseded := variant("superseded", 20, 1, 1, "superseded-by")
	audit := variant("audit-no-change", 40, 1, -1, "no-change-audit", "reviewed-paths")
	one := 1
	auditEvidence, ok := audit.Properties["evidence"]
	if !ok || auditEvidence == nil {
		panic("audit close schema has no evidence property")
	}
	auditEvidence.Contains = evidenceVariant("no-change-audit")
	auditEvidence.MinContains = &one
	auditEvidence.MaxContains = &one
	return []*jsonschema.Schema{done, wontfix, duplicate, superseded, audit}
}

func toolHints(readOnly, destructive, openWorld bool) *sdkmcp.ToolAnnotations {
	return &sdkmcp.ToolAnnotations{
		DestructiveHint: &destructive,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
		ReadOnlyHint:    readOnly,
	}
}

// nonIdempotent marks a creation tool whose identical retry mints a new
// record because no idempotency key or natural unique key deduplicates it.
func nonIdempotent(hints *sdkmcp.ToolAnnotations) *sdkmcp.ToolAnnotations {
	hints.IdempotentHint = false
	return hints
}

// SearchInput selects matching issues without returning large issue bodies.
type SearchInput struct {
	Project       string   `json:"project,omitempty" jsonschema:"Project name; omit to search every project in scope"`
	Query         string   `json:"query" jsonschema:"Non-empty text to search for"`
	Mode          string   `json:"mode,omitempty" jsonschema:"Search mode: auto, lexical, hybrid, or semantic"`
	Limit         int      `json:"limit,omitempty" jsonschema:"Maximum results from 1 through 100; default 20"`
	Labels        []string `json:"labels,omitempty" jsonschema:"Labels that every result must have"`
	ExcludeLabels []string `json:"exclude_labels,omitempty" jsonschema:"Labels that results must not have"`
}

// ListInput filters project issues. Empty status means open and closed.
type ListInput struct {
	Project       string   `json:"project,omitempty" jsonschema:"Project name; omit to list every project in scope"`
	Status        string   `json:"status,omitempty" jsonschema:"Issue status: open, closed, or empty for both"`
	Limit         int      `json:"limit,omitempty" jsonschema:"Maximum results from 1 through 100; default 20"`
	Priority      *int64   `json:"priority,omitempty" jsonschema:"Exact priority from 0 through 4"`
	MaxPriority   *int64   `json:"max_priority,omitempty" jsonschema:"Highest numeric priority from 0 through 4"`
	Owner         string   `json:"owner,omitempty" jsonschema:"Only issues owned by this actor"`
	Unowned       bool     `json:"unowned,omitempty" jsonschema:"Only issues with no owner"`
	Labels        []string `json:"labels,omitempty" jsonschema:"Labels that every issue must have"`
	ExcludeLabels []string `json:"exclude_labels,omitempty" jsonschema:"Labels that issues must not have"`
	Metadata      []string `json:"metadata,omitempty" jsonschema:"Metadata filters as key or key=value"`
}

// ReadyInput filters unblocked open work.
type ReadyInput struct {
	Project       string   `json:"project,omitempty" jsonschema:"Project name; omit to list ready work in every project in scope"`
	Limit         int      `json:"limit,omitempty" jsonschema:"Maximum results from 1 through 100; default 20"`
	Owner         string   `json:"owner,omitempty" jsonschema:"Only issues owned by this actor"`
	Unowned       bool     `json:"unowned,omitempty" jsonschema:"Only issues with no owner"`
	Labels        []string `json:"labels,omitempty" jsonschema:"Labels that every issue must have"`
	ExcludeLabels []string `json:"exclude_labels,omitempty" jsonschema:"Labels that issues must not have"`
}

// LabelsInput selects one project or aggregates across the startup scope.
type LabelsInput struct {
	Project string `json:"project,omitempty" jsonschema:"Project name; omit to list labels in every project in scope"`
}

// ShowInput selects one bound-project issue.
type ShowInput struct {
	Ref          string `json:"ref" jsonschema:"Bare short ID, qualified reference, or full issue UID"`
	CommentLimit int    `json:"comment_limit,omitempty" jsonschema:"Maximum newest comments from 1 through 100; omit for 20"`
}

// CreateInput creates one issue and its initial relationships.
type CreateInput struct {
	Project        string         `json:"project,omitempty" jsonschema:"Target project; required in multi-project modes"`
	Title          string         `json:"title" jsonschema:"Concise issue title"`
	Body           string         `json:"body,omitempty" jsonschema:"Issue context, reason, and acceptance details"`
	Owner          string         `json:"owner,omitempty" jsonschema:"Initial owner"`
	Priority       *int64         `json:"priority,omitempty" jsonschema:"Priority from 0 through 4"`
	Labels         []string       `json:"labels,omitempty" jsonschema:"Initial project labels"`
	Metadata       map[string]any `json:"metadata,omitempty" jsonschema:"Initial issue metadata"`
	ScheduledOn    string         `json:"scheduled_on,omitempty" jsonschema:"Schedule as YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or an RFC 3339 UTC instant ending in Z"`
	Timezone       string         `json:"timezone,omitempty" jsonschema:"IANA timezone for date-only and local scheduled_on values"`
	ForceNew       bool           `json:"force_new,omitempty" jsonschema:"Create even when Kata finds a likely duplicate"`
	Parent         string         `json:"parent,omitempty" jsonschema:"Parent issue reference"`
	Blocks         []string       `json:"blocks,omitempty" jsonschema:"Issues this new issue blocks"`
	BlockedBy      []string       `json:"blocked_by,omitempty" jsonschema:"Issues that block this new issue"`
	Related        []string       `json:"related,omitempty" jsonschema:"Related issue references"`
	IdempotencyKey string         `json:"idempotency_key" jsonschema:"Stable unique key for safe retries"`
}

// EditInput applies issue field and relationship deltas atomically.
type EditInput struct {
	Ref              string         `json:"ref" jsonschema:"Issue reference; use project#ref in multi-project mode"`
	Title            *string        `json:"title,omitempty" jsonschema:"Replacement title"`
	Body             *string        `json:"body,omitempty" jsonschema:"Replacement body"`
	Owner            *string        `json:"owner,omitempty" jsonschema:"Replacement owner"`
	ClearOwner       bool           `json:"clear_owner,omitempty" jsonschema:"Remove the current owner"`
	Priority         *int64         `json:"priority,omitempty" jsonschema:"Replacement priority from 0 through 4"`
	ClearPriority    bool           `json:"clear_priority,omitempty" jsonschema:"Remove the current priority"`
	Metadata         map[string]any `json:"metadata,omitempty" jsonschema:"Issue metadata merge patch; JSON null removes a key"`
	ScheduledOn      *string        `json:"scheduled_on,omitempty" jsonschema:"Schedule as YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or an RFC 3339 UTC instant ending in Z"`
	Timezone         *string        `json:"timezone,omitempty" jsonschema:"IANA timezone for date-only and local scheduled_on values"`
	ClearScheduledOn bool           `json:"clear_scheduled_on,omitempty" jsonschema:"Remove scheduled_on"`
	ClearTimezone    bool           `json:"clear_timezone,omitempty" jsonschema:"Remove timezone"`
	Parent           *string        `json:"parent,omitempty" jsonschema:"Replacement parent reference"`
	RemoveParent     *string        `json:"remove_parent,omitempty" jsonschema:"Current parent reference to remove strictly"`
	AddBlocks        []string       `json:"add_blocks,omitempty" jsonschema:"Issue references to add as blocked by this issue"`
	RemoveBlocks     []string       `json:"remove_blocks,omitempty" jsonschema:"Blocking relationships to remove"`
	AddBlockedBy     []string       `json:"add_blocked_by,omitempty" jsonschema:"Issue references that block this issue"`
	RemoveBlockedBy  []string       `json:"remove_blocked_by,omitempty" jsonschema:"Blocked-by relationships to remove"`
	AddRelated       []string       `json:"add_related,omitempty" jsonschema:"Related issue references to add"`
	RemoveRelated    []string       `json:"remove_related,omitempty" jsonschema:"Related issue references to remove"`
}

// CommentInput appends an idempotent comment.
type CommentInput struct {
	Ref            string `json:"ref" jsonschema:"Issue reference; use project#ref in multi-project mode"`
	Body           string `json:"body" jsonschema:"Comment body"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"Stable unique key for safe retries"`
}

// ClaimInput claims an issue, optionally replacing its current owner.
type ClaimInput struct {
	Ref   string `json:"ref" jsonschema:"Issue reference; use project#ref in multi-project mode"`
	Force bool   `json:"force,omitempty" jsonschema:"Replace a different current owner"`
}

// SetLabelInput makes label presence explicit and naturally idempotent.
type SetLabelInput struct {
	Ref     string `json:"ref" jsonschema:"Issue reference; use project#ref in multi-project mode"`
	Label   string `json:"label" jsonschema:"Project label"`
	Present bool   `json:"present" jsonschema:"True to add the label; false to remove it"`
}

// SetDeadlineInput sets or clears the deadline_on metadata primitive.
type SetDeadlineInput struct {
	Ref           string  `json:"ref" jsonschema:"Issue reference in the bound project"`
	Deadline      *string `json:"deadline,omitempty" jsonschema:"Deadline date, local date-time, or RFC 3339 UTC instant ending in Z"`
	ClearDeadline bool    `json:"clear_deadline,omitempty" jsonschema:"Remove the current deadline"`
	Revision      *int64  `json:"revision,omitempty" jsonschema:"Required current issue revision for a conditional write"`
}

// SetScheduleInput sets or clears the scheduled_on metadata primitive.
type SetScheduleInput struct {
	Ref           string  `json:"ref" jsonschema:"Issue reference in the bound project"`
	Schedule      *string `json:"schedule,omitempty" jsonschema:"Schedule date, local date-time, or RFC 3339 UTC instant ending in Z"`
	ClearSchedule bool    `json:"clear_schedule,omitempty" jsonschema:"Remove the current schedule gate"`
	Revision      *int64  `json:"revision,omitempty" jsonschema:"Required current issue revision for a conditional write"`
}

// SetMetadataInput patches several keys in one operation.
type SetMetadataInput struct {
	Ref      string         `json:"ref" jsonschema:"Issue reference; use project#ref in multi-project mode"`
	Patch    map[string]any `json:"patch" jsonschema:"Metadata values to set; JSON null removes a key"`
	Revision *int64         `json:"revision,omitempty" jsonschema:"Required current issue revision for a conditional write"`
}

// Evidence is a typed completion proof accepted by Kata close.
type Evidence struct {
	Type      string   `json:"type" jsonschema:"Evidence type: commit, pr, test, reviewed-paths, no-change-audit, duplicate-of, or superseded-by"`
	SHA       string   `json:"sha,omitempty" jsonschema:"Commit SHA for commit evidence"`
	URL       string   `json:"url,omitempty" jsonschema:"Pull request URL for pr evidence"`
	Command   string   `json:"command,omitempty" jsonschema:"Executed command for test evidence"`
	Paths     []string `json:"paths,omitempty" jsonschema:"Reviewed paths for reviewed-paths evidence"`
	Rationale string   `json:"rationale,omitempty" jsonschema:"Rationale for no-change-audit evidence"`
	IssueRef  string   `json:"issue_ref,omitempty" jsonschema:"Target issue for duplicate-of or superseded-by evidence"`
}

// CloseInput makes the completion assertion explicit.
type CloseInput struct {
	Ref      string     `json:"ref" jsonschema:"Issue reference; use project#ref in multi-project mode"`
	Reason   string     `json:"reason" jsonschema:"Close reason: done, wontfix, duplicate, superseded, or audit-no-change"`
	Message  string     `json:"message" jsonschema:"Substantive completion message"`
	Evidence []Evidence `json:"evidence" jsonschema:"Typed evidence that supports the close"`
	DryRun   bool       `json:"dry_run,omitempty" jsonschema:"Validate the close without changing the issue"`
}

// ReopenInput reactivates closed work.
type ReopenInput struct {
	Ref string `json:"ref" jsonschema:"Issue reference; use project#ref in multi-project mode"`
}

// ProjectIdentity identifies the immutable startup project.
type ProjectIdentity struct {
	ID   int64  `json:"id"`
	UID  string `json:"uid,omitempty"`
	Name string `json:"name"`
}

// IssueSummary is the compact issue form used by list-like tools.
type IssueSummary struct {
	UID          string    `json:"uid"`
	Ref          string    `json:"ref"`
	QualifiedRef string    `json:"qualified_ref"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	Owner        *string   `json:"owner,omitempty"`
	Priority     *int64    `json:"priority,omitempty"`
	Labels       *[]string `json:"labels,omitempty"`
	Blocked      *bool     `json:"blocked,omitempty"`
	Revision     int64     `json:"revision"`
	UpdatedAt    string    `json:"updated_at"`
	ScheduledOn  *string   `json:"scheduled_on,omitempty"`
	Timezone     *string   `json:"timezone,omitempty"`
	updatedAt    time.Time
}

// IssueListOutput is a bounded collection without issue bodies or comments.
type IssueListOutput struct {
	Project   *ProjectIdentity  `json:"project,omitempty"`
	Projects  []ProjectIdentity `json:"projects,omitempty"`
	Issues    []IssueSummary    `json:"issues"`
	Truncated bool              `json:"truncated"`
}

// SearchHit adds relevance information to a compact issue.
type SearchHit struct {
	Issue     IssueSummary `json:"issue"`
	Score     float64      `json:"score"`
	MatchedIn []string     `json:"matched_in"`
}

// SearchOutput reports the effective mode and any semantic fallback.
type SearchOutput struct {
	Project        *ProjectIdentity  `json:"project,omitempty"`
	Projects       []ProjectIdentity `json:"projects,omitempty"`
	Query          string            `json:"query"`
	Mode           string            `json:"mode"`
	Degraded       bool              `json:"degraded"`
	DegradedReason string            `json:"degraded_reason,omitempty"`
	Results        []SearchHit       `json:"results"`
	Truncated      bool              `json:"truncated"`
}

// LabelCount reports project label reuse.
type LabelCount struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// LabelsOutput is the project label catalog.
type LabelsOutput struct {
	Project  *ProjectIdentity  `json:"project,omitempty"`
	Projects []ProjectIdentity `json:"projects,omitempty"`
	Labels   []LabelCount      `json:"labels"`
}

// CommentSummary is a bounded comment representation.
type CommentSummary struct {
	UID       string `json:"uid"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// LinkSummary identifies a relationship endpoint.
type LinkSummary struct {
	Type         string `json:"type"`
	QualifiedRef string `json:"qualified_ref"`
	Status       string `json:"status"`
}

// IssueDetail is the full bounded result for kata.show.
type IssueDetail struct {
	IssueSummary
	Labels   []string         `json:"labels"`
	Body     string           `json:"body"`
	Author   string           `json:"author"`
	Metadata map[string]any   `json:"metadata"`
	Links    []LinkSummary    `json:"links"`
	Comments []CommentSummary `json:"comments"`
}

// ShowOutput wraps one issue with its project identity.
type ShowOutput struct {
	Project           ProjectIdentity `json:"project"`
	Issue             IssueDetail     `json:"issue"`
	CommentsTruncated bool            `json:"comments_truncated"`
}

// EventSummary records the actual attributed event produced by a mutation.
type EventSummary struct {
	UID   string `json:"uid"`
	Type  string `json:"type"`
	Actor string `json:"actor"`
}

// LinkPeerSummary identifies a relationship change endpoint.
type LinkPeerSummary struct {
	UID          string `json:"uid"`
	Ref          string `json:"ref"`
	QualifiedRef string `json:"qualified_ref"`
	Status       string `json:"status"`
}

// LinkChangesSummary reports the relationship deltas applied by one edit.
type LinkChangesSummary struct {
	BlockedByAdded   []LinkPeerSummary `json:"blocked_by_added,omitempty"`
	BlockedByRemoved []LinkPeerSummary `json:"blocked_by_removed,omitempty"`
	BlocksAdded      []LinkPeerSummary `json:"blocks_added,omitempty"`
	BlocksRemoved    []LinkPeerSummary `json:"blocks_removed,omitempty"`
	ParentRemoved    *LinkPeerSummary  `json:"parent_removed,omitempty"`
	ParentSet        *LinkPeerSummary  `json:"parent_set,omitempty"`
	RelatedAdded     []LinkPeerSummary `json:"related_added,omitempty"`
	RelatedRemoved   []LinkPeerSummary `json:"related_removed,omitempty"`
}

// MutationOutput is the common mutation result.
type MutationOutput struct {
	Project       ProjectIdentity `json:"project"`
	Issue         IssueSummary    `json:"issue"`
	Changed       bool            `json:"changed"`
	Reused        *bool           `json:"reused,omitempty"`
	Event         *EventSummary   `json:"event,omitempty"`
	PreviousOwner *string         `json:"previous_owner,omitempty"`
}

// EditOutput preserves every ordered event and applied relationship change.
type EditOutput struct {
	Project ProjectIdentity     `json:"project"`
	Issue   IssueSummary        `json:"issue"`
	Changed bool                `json:"changed"`
	Reused  *bool               `json:"reused,omitempty"`
	Event   *EventSummary       `json:"event,omitempty"`
	Events  []EventSummary      `json:"events"`
	Changes *LinkChangesSummary `json:"changes,omitempty"`
}

// CommentOutput reports the appended or reused comment.
type CommentOutput struct {
	Project ProjectIdentity `json:"project"`
	Issue   IssueSummary    `json:"issue"`
	Comment CommentSummary  `json:"comment"`
	Changed bool            `json:"changed"`
	Reused  *bool           `json:"reused,omitempty"`
	Event   *EventSummary   `json:"event,omitempty"`
}

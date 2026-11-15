package mcpserver

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
	"golang.org/x/sync/singleflight"
)

const (
	toolSearchQueryBytes  = 512
	toolSearchMaxResults  = 10
	toolSearchOutputBytes = 32 * 1024
)

// ToolSearchInput describes a task, rather than an issue-data query.
type ToolSearchInput struct {
	Query string `json:"query"`
	Limit *int   `json:"limit,omitempty"`
}

// ToolSearchParameter summarizes a top-level input. Full constraints stay in
// tools/list, including conditional requirements and nested object schemas.
type ToolSearchParameter struct {
	Name     string   `json:"name"`
	Types    []string `json:"types"`
	Required bool     `json:"required"`
}

// ToolSearchResult points to one real operation and its existing load route.
type ToolSearchResult struct {
	Name    string                `json:"name"`
	Summary string                `json:"summary"`
	Section string                `json:"section,omitempty"`
	Loader  string                `json:"loader,omitempty"`
	Guides  []string              `json:"guides"`
	Inputs  []ToolSearchParameter `json:"inputs"`
}

// ToolSearchOutput returns bounded task matches and the complete-schema route.
type ToolSearchOutput struct {
	Query       string             `json:"query"`
	Results     []ToolSearchResult `json:"results"`
	SchemaRoute string             `json:"schema_route"`
	Truncated   bool               `json:"truncated"`
}

type toolSearchCatalog struct {
	tools  map[string]ToolSearchResult
	titles map[string]string
}

// toolSearchCatalogLoader shares one in-flight build and caches only success,
// so a transient initialization failure can be retried by a later search.
type toolSearchCatalogLoader struct {
	catalog atomic.Pointer[toolSearchCatalog]
	group   singleflight.Group
	build   func() (*toolSearchCatalog, error)
}

// get stops waiting when ctx ends so a cancelled search frees its tool-call
// slot. The build itself is shared and bounded by its own deadline, so it keeps
// running for other searches and is not tied to any one caller's context.
func (loader *toolSearchCatalogLoader) get(ctx context.Context) (*toolSearchCatalog, error) {
	if catalog := loader.catalog.Load(); catalog != nil {
		return catalog, nil
	}
	built := loader.group.DoChan("catalog", func() (any, error) {
		if catalog := loader.catalog.Load(); catalog != nil {
			return catalog, nil
		}
		catalog, err := loader.build()
		if err != nil {
			return nil, err
		}
		loader.catalog.Store(catalog)
		return catalog, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-built:
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.(*toolSearchCatalog), nil
	}
}

func registerToolSearch(server *sdkmcp.Server, options Options) {
	// A private SDK catalog reuses the actual typed registrations, without
	// activating public sections or calling any operation.
	registerToolSearchCatalog(server, &toolSearchCatalogLoader{build: func() (*toolSearchCatalog, error) {
		return newToolSearchCatalog(options)
	}})
}

func registerToolSearchCatalog(server *sdkmcp.Server, catalog *toolSearchCatalogLoader) {
	addTool(server, "kata.search_tools", "Find Kata tools", "Find typed MCP tools for a task, with loader, guide and input details. Searches tool metadata only; kata.search searches issues. Available before loading sections.", toolHints(true, false, false),
		func(ctx context.Context, request *sdkmcp.CallToolRequest, input ToolSearchInput) (*sdkmcp.CallToolResult, ToolSearchOutput, error) {
			// The SDK repairs malformed Unicode while validating its typed
			// copy. Decode the retained original strictly before using it.
			if err := json.Unmarshal(request.Params.Arguments, &input); err != nil {
				return nil, ToolSearchOutput{}, err
			}
			if err := validateToolSearch(input); err != nil {
				return nil, ToolSearchOutput{}, err
			}
			index, err := catalog.get(ctx)
			if err != nil {
				return nil, ToolSearchOutput{}, fmt.Errorf("tool metadata catalog is temporarily unavailable, retry the search: %w", err)
			}
			return successResult(), index.search(input), nil
		})
}

func validateToolSearch(input ToolSearchInput) error {
	if !utf8.ValidString(input.Query) || len(input.Query) > toolSearchQueryBytes || strings.TrimSpace(input.Query) == "" {
		return errors.New("query must be non-blank UTF-8 text of at most 512 bytes")
	}
	if input.Limit != nil && (*input.Limit < 1 || *input.Limit > toolSearchMaxResults) {
		return errors.New("limit must be between 1 and 10")
	}
	return nil
}

func newToolSearchCatalog(options Options) (*toolSearchCatalog, error) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "kata-tool-metadata", Version: options.Version}, nil)
	sections := toolSections(options.StorageAdmin != nil, options.EnableTokenAdmin && options.Scope.Mode() == ScopeAll)
	for _, section := range sections {
		if section.available {
			section.register(server, toolHandlers{options: options})
		}
	}
	registerSkillsTool(server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect tool metadata catalog: %w", err)
	}
	defer func() { _ = serverSession.Close() }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "kata-tool-discovery", Version: options.Version}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize tool metadata catalog: %w", err)
	}
	defer func() { _ = session.Close() }()
	index := &toolSearchCatalog{tools: make(map[string]ToolSearchResult), titles: make(map[string]string)}
	for cursor := ""; ; {
		page, err := session.ListTools(ctx, &sdkmcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("list tool metadata: %w", err)
		}
		for _, tool := range page.Tools {
			if !toolSearchInScope(tool.Name, options.Scope.Mode()) {
				continue
			}
			inputs, err := toolSearchInputs(tool.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("read %s input metadata: %w", tool.Name, err)
			}
			result := ToolSearchResult{Name: tool.Name, Summary: tool.Description, Guides: toolSearchGuides(tool.Name), Inputs: inputs}
			for _, section := range sections {
				if slices.Contains(section.tools, tool.Name) {
					result.Section, result.Loader = section.section, section.loader
					break
				}
			}
			index.tools[tool.Name], index.titles[tool.Name] = result, tool.Title
		}
		cursor = page.NextCursor
		if cursor == "" {
			return index, nil
		}
	}
}

// These operations unconditionally require daemon-wide scope in their
// handlers. Keep partially supported operations (for example sync_update and
// federation_leave) discoverable; their full schemas/guides describe options.
// This filters suggestions only. The handlers remain the authority boundary.
func toolSearchInScope(name string, mode ScopeMode) bool {
	if mode == ScopeAll {
		return true
	}
	switch name {
	case "kata.storage_export", "kata.project_create", "kata.project_update", "kata.project_merge",
		"kata.project_remove", "kata.project_restore", "kata.project_purge",
		"kata.connectors", "kata.connector_fields", "kata.connector_field_map",
		"kata.connector_field_unmap", "kata.bridge_bind",
		"kata.federation_bridge_connect", "kata.federation_bridge_status", "kata.federation_bridge_disconnect",
		"kata.federation_spoke_name", "kata.federation_rebind", "kata.federation_root_key_repin":
		return false
	}
	return true
}

func toolSearchInputs(raw any) ([]ToolSearchParameter, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return nil, err
	}
	inputs := make([]ToolSearchParameter, 0, len(schema.Properties))
	for name, property := range schema.Properties {
		types := append([]string{}, property.Types...)
		if property.Type != "" {
			types = append(types, property.Type)
		}
		slices.Sort(types)
		inputs = append(inputs, ToolSearchParameter{Name: name, Types: types, Required: slices.Contains(schema.Required, name)})
	}
	slices.SortFunc(inputs, func(a, b ToolSearchParameter) int { return strings.Compare(a.Name, b.Name) })
	return inputs, nil
}

func toolSearchGuides(tool string) []string {
	names := make([]string, 0)
	for _, guide := range skillGuides {
		if slices.Contains(guide.tools, tool) {
			names = append(names, guide.name)
		}
	}
	return names
}

func (catalog *toolSearchCatalog) search(input ToolSearchInput) ToolSearchOutput {
	limit := 5
	if input.Limit != nil {
		limit = *input.Limit
	}
	query := strings.TrimSpace(input.Query)
	output := ToolSearchOutput{Query: query, Results: make([]ToolSearchResult, 0, limit), SchemaRoute: "Call the result's loader if present, refresh tools/list, and read its inputSchema for full constraints and nested inputs. Call kata.skills with a returned guide name for guidance."}
	// Kit may lazily sort the tree: build one per call so concurrent searches
	// never mutate shared command state. These commands contain no CLI logic.
	root := &cobra.Command{Use: "kata"}
	// The CLI evaluation already established these task words for the
	// corresponding operations. MCP adds search/guide terms because those
	// routes are otherwise easy to confuse with import and cron workflows.
	keywords := map[string]string{
		"kata.comment": "log note", "kata.create": "bug", "kata.show": "details",
		"kata.ready": "unblocked", "kata.close": "completed", "kata.cron_run": "recorded",
		"kata.search": "find lookup", "kata.skills": "agent workflow guide",
	}
	for name, tool := range catalog.tools {
		// Titles are searchable names too; otherwise compound MCP names such
		// as import_issues disproportionately match the common word "issue".
		command := &cobra.Command{Use: strings.TrimPrefix(name, "kata."), Aliases: []string{catalog.titles[name], keywords[name]}, Short: tool.Summary, Run: func(*cobra.Command, []string) {}}
		clisearch.SetKeywords(command, catalog.titles[name], keywords[name])
		root.AddCommand(command)
	}
	candidates := make([]ToolSearchResult, 0, toolSearchMaxResults+1)
	if tool, ok := catalog.tools[query]; ok {
		candidates = append(candidates, tool)
	}
	for _, match := range clisearch.Search(root, query, toolSearchMaxResults, clisearch.Options{Synonyms: map[string][]string{
		"ticket": {"issue"}, "bug": {"issue"}, "todo": {"issue"}, "note": {"comment"},
	}}) {
		name := "kata." + strings.TrimPrefix(match.Command, "kata ")
		if name == query {
			continue
		}
		candidates = append(candidates, catalog.tools[name])
	}
	for _, candidate := range candidates {
		output.Results = append(output.Results, candidate)
		// Match the SDK's JSON-v1-compatible escaping when measuring the
		// wire payload. Only strings, booleans and slices can reach here.
		encoded, _ := json.Marshal(output, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
		if len(encoded) > toolSearchOutputBytes {
			output.Results = output.Results[:len(output.Results)-1]
			output.Truncated = true
			break
		}
		if len(output.Results) == limit {
			break
		}
	}
	return output
}

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	"pgregory.net/rapid"

	"go.kenn.io/kata/internal/storageadmin"
)

type discoveredInput struct {
	Name     string   `json:"name"`
	Types    []string `json:"types"`
	Required bool     `json:"required"`
}

type discoveredTool struct {
	Name    string            `json:"name"`
	Summary string            `json:"summary"`
	Section string            `json:"section,omitempty"`
	Loader  string            `json:"loader,omitempty"`
	Guides  []string          `json:"guides"`
	Inputs  []discoveredInput `json:"inputs"`
}

type discoveryResponse struct {
	Query       string           `json:"query"`
	Results     []discoveredTool `json:"results"`
	SchemaRoute string           `json:"schema_route"`
	Truncated   bool             `json:"truncated"`
}

func searchToolSession(t *testing.T, session *sdkmcp.ClientSession, query string, limit int) discoveryResponse {
	t.Helper()
	arguments := map[string]any{"query": query}
	if limit != 0 {
		arguments["limit"] = limit
	}
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.search_tools", Arguments: arguments})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	var output discoveryResponse
	require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
	require.NotNil(t, output.Results)
	return output
}

func discoveryOptions(t *testing.T) Options {
	return Options{Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test-version"}
}

// Product contract: a transient failure building the private metadata catalog
// must not disable task discovery for the rest of the server's lifetime.
func TestToolSearchRetriesFailedCatalogBuild(t *testing.T) {
	catalog, err := newToolSearchCatalog(discoveryOptions(t))
	require.NoError(t, err)
	attempts := 0
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test-server", Version: "1"}, nil)
	registerToolSearchCatalog(server, &toolSearchCatalogLoader{build: func() (*toolSearchCatalog, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("transient catalog failure")
		}
		return catalog, nil
	}})
	session := connectServerSession(t, server, nil, nil)

	failed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "kata.search_tools", Arguments: map[string]any{"query": "add a comment to an issue"},
	})
	require.NoError(t, err)
	require.True(t, failed.IsError)
	require.Len(t, failed.Content, 1)
	text, ok := failed.Content[0].(*sdkmcp.TextContent)
	require.True(t, ok)
	require.Equal(t, "tool metadata catalog is temporarily unavailable, retry the search: transient catalog failure", text.Text)

	for range 2 {
		output := searchToolSession(t, session, "add a comment to an issue", 1)
		require.Len(t, output.Results, 1)
		require.Equal(t, "kata.comment", output.Results[0].Name)
	}
	require.Equal(t, 2, attempts)
}

// Product contract: concurrent searches on a cold server share one catalog
// build, because each build registers every section on a private server.
func TestToolSearchCatalogSharesConcurrentBuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		want := &toolSearchCatalog{}
		release := make(chan struct{})
		var builds atomic.Int32
		loader := &toolSearchCatalogLoader{build: func() (*toolSearchCatalog, error) {
			builds.Add(1)
			<-release
			return want, nil
		}}
		var waiters sync.WaitGroup
		catalogs := make([]*toolSearchCatalog, 8)
		errs := make([]error, len(catalogs))
		for i := range catalogs {
			waiters.Go(func() { catalogs[i], errs[i] = loader.get(t.Context()) })
		}
		synctest.Wait()
		close(release)
		waiters.Wait()

		require.EqualValues(t, 1, builds.Load())
		for i, catalog := range catalogs {
			require.NoError(t, errs[i])
			require.Same(t, want, catalog)
		}
	})
}

// Product contract: a cancelled search gives back its tool-call slot while a
// slow catalog build continues for the searches that still want its result.
func TestToolSearchCancelledWaitReleasesAdmissionSlot(t *testing.T) {
	catalog, err := newToolSearchCatalog(discoveryOptions(t))
	require.NoError(t, err)
	started := make(chan struct{}, 8)
	admitted := make(chan struct{}, 8)
	blockBuild := make(chan struct{})
	releaseBuild := sync.OnceFunc(func() { close(blockBuild) })
	var builds atomic.Int32
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test-server", Version: "1"}, nil)
	server.AddReceivingMiddleware(toolAdmissionMiddleware(rate.NewLimiter(rate.Inf, 1), make(chan struct{}, 1), func() { admitted <- struct{}{} }))
	registerToolSearchCatalog(server, &toolSearchCatalogLoader{build: func() (*toolSearchCatalog, error) {
		builds.Add(1)
		started <- struct{}{}
		<-blockBuild
		return catalog, nil
	}})
	session := connectServerSession(t, server, nil, nil)
	t.Cleanup(releaseBuild)
	params := &sdkmcp.CallToolParams{Name: "kata.search_tools", Arguments: map[string]any{"query": "add a comment to an issue", "limit": 1}}
	receive := func(signal <-chan struct{}, failure string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatal(failure)
		}
	}

	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = session.CallTool(firstCtx, params)
	}()
	receive(admitted, "first search was not admitted")
	receive(started, "first search did not start the catalog build")
	cancelFirst()
	receive(firstDone, "cancelled search did not return to its client")

	type callResult struct {
		result *sdkmcp.CallToolResult
		err    error
	}
	secondDone := make(chan callResult, 1)
	go func() {
		result, err := session.CallTool(t.Context(), params)
		secondDone <- callResult{result: result, err: err}
	}()
	receive(admitted, "cancelled search kept the only tool-call slot")
	releaseBuild()

	select {
	case second := <-secondDone:
		require.NoError(t, second.err)
		require.False(t, second.result.IsError, "%s", mustJSON(t, second.result))
	case <-time.After(5 * time.Second):
		t.Fatal("second search did not receive the shared catalog")
	}
	require.EqualValues(t, 1, builds.Load())
}

// Product contract: a cold MCP-only client can find real operations without
// invoking issue APIs or loading every section into its public catalog.
func TestToolSearchColdStartAndLoadRoutes(t *testing.T) {
	changed := make(chan struct{}, 1)
	session := connectRawTestServerWithClientOptions(t, discoveryOptions(t), &sdkmcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdkmcp.ToolListChangedRequest) {
			// The test needs a notification signal, not a count. Coalesce repeated
			// list-changed notifications so the callback cannot block the client.
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	})
	before, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.NotContains(t, toolNames(before.Tools), "kata.comment")
	output := searchToolSession(t, session, "add a comment to an issue", 1)
	require.Len(t, output.Results, 1)
	comment := output.Results[0]
	require.Equal(t, "kata.comment", comment.Name)
	require.Equal(t, "issue_mutation", comment.Section)
	require.Equal(t, "kata.load_issue_mutation", comment.Loader)
	require.Contains(t, comment.Guides, "issues")
	require.Contains(t, comment.Inputs, discoveredInput{Name: "idempotency_key", Types: []string{"string"}, Required: true})
	require.Contains(t, comment.Inputs, discoveredInput{Name: "ref", Types: []string{"string"}, Required: true})
	require.Contains(t, output.SchemaRoute, "tools/list")
	afterSearch, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, before.Tools, afterSearch.Tools, "search must not activate public sections")
	loaded, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: comment.Loader})
	require.NoError(t, err)
	require.False(t, loaded.IsError)
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("loader did not announce its catalog change")
	}
	afterLoad, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range afterLoad.Tools {
		if tool.Name == comment.Name {
			require.Equal(t, tool.Description, comment.Summary)
			return
		}
	}
	t.Fatal("discovered operation is absent after loading its real section")
}

func TestToolSearchTaskRanking(t *testing.T) {
	session := connectRawTestServerWithOptions(t, discoveryOptions(t))
	for _, test := range []struct{ query, want string }{
		{"add a comment to an issue", "kata.comment"},
		{"claim an issue", "kata.claim"},
		{"close completed work with evidence", "kata.close"},
		{"find unblocked issues", "kata.ready"},
		{"find issues about a bug", "kata.search"},
		{"set an issue deadline", "kata.set_deadline"},
		{"patch issue metadata", "kata.set_metadata"},
		{"read recorded cron run", "kata.cron_run"},
		{"read the agent workflow guide", "kata.skills"},
		{"kata.comment", "kata.comment"},
	} {
		t.Run(test.query, func(t *testing.T) {
			output := searchToolSession(t, session, test.query, 3)
			require.NotEmpty(t, output.Results)
			require.Equal(t, test.want, output.Results[0].Name)
		})
	}
}

func TestToolSearchInputAndEmptyResults(t *testing.T) {
	session := connectRawTestServerWithOptions(t, discoveryOptions(t))
	for _, args := range []map[string]any{
		{}, {"query": ""}, {"query": " \t\n\u2003"},
		{"query": strings.Repeat("a", 513)}, {"query": strings.Repeat("猫", 171)},
		{"query": "comment", "limit": 0}, {"query": "comment", "limit": -1},
		{"query": "comment", "limit": 11}, {"query": "comment", "limit": 1.5},
		{"query": "comment", "limit": nil},
		{"query": true}, {"query": "comment", "limit": "1"},
	} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.search_tools", Arguments: args})
		require.NoError(t, err)
		require.True(t, result.IsError, "%s", mustJSON(t, args))
		require.Nil(t, result.StructuredContent)
	}
	for _, query := range []string{"___", "the and to", "xyzzynotatool"} {
		output := searchToolSession(t, session, query, 0)
		require.Empty(t, output.Results)
		require.False(t, output.Truncated)
	}
	output := searchToolSession(t, session, "issue", 0)
	require.Len(t, output.Results, 5)
	for _, result := range output.Results {
		require.True(t, strings.HasPrefix(result.Name, "kata."))
		require.False(t, strings.HasPrefix(result.Name, "kata.load_"))
		require.True(t, slices.IsSortedFunc(result.Inputs, func(a, b discoveredInput) int { return strings.Compare(a.Name, b.Name) }))
	}
}

func TestToolSearchLoadedAndStartupEquivalence(t *testing.T) {
	options := discoveryOptions(t)
	cold := connectRawTestServerWithOptions(t, options)
	query := "close work with evidence"
	want := searchToolSession(t, cold, query, 0)
	for _, section := range []string{"issue_discovery", "issue_mutation", "issue_lifecycle", "coordination"} {
		result, err := cold.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_" + section})
		require.NoError(t, err)
		require.False(t, result.IsError)
		options.StartupSections = append(options.StartupSections, section)
	}
	require.Equal(t, want, searchToolSession(t, cold, query, 0))
	startup := connectRawTestServerWithOptions(t, options)
	require.Equal(t, want, searchToolSession(t, startup, query, 0))
}

func TestToolSearchFiltersAdminAndScope(t *testing.T) {
	admin, err := storageadmin.New(storageadmin.Config{Root: t.TempDir(), SourceDSN: "source.db"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	bound, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	allowlist, err := NewAllowlistScope([]ProjectIdentity{{ID: 42, Name: "spoke-project", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}})
	require.NoError(t, err)
	for _, scope := range []*Scope{bound, allowlist, NewAllScope()} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", scope.Mode(), enabled), func(t *testing.T) {
				options := discoveryOptions(t)
				options.Scope, options.EnableTokenAdmin = scope, enabled
				if enabled {
					options.StorageAdmin = admin
				}
				session := connectRawTestServerWithOptions(t, options)
				for _, name := range []string{"kata.token_create", "kata.team_create", "kata.storage_export"} {
					output := searchToolSession(t, session, name, 10)
					found := slices.ContainsFunc(output.Results, func(result discoveredTool) bool { return result.Name == name })
					require.Equal(t, enabled && scope.Mode() == ScopeAll, found, name)
				}
				output := searchToolSession(t, session, "kata.storage_import", 1)
				found := slices.ContainsFunc(output.Results, func(result discoveredTool) bool { return result.Name == "kata.storage_import" })
				require.Equal(t, enabled, found, "storage import supports every project scope when configured")
				for _, name := range []string{"kata.project_create", "kata.project_merge", "kata.connector_field_map", "kata.federation_rebind", "kata.federation_bridge_status"} {
					output := searchToolSession(t, session, name, 10)
					found := slices.ContainsFunc(output.Results, func(result discoveredTool) bool { return result.Name == name })
					require.Equal(t, scope.Mode() == ScopeAll, found, name)
				}
			})
		}
	}
}

// Generated task text and exact tool names exercise bounds, determinism,
// availability and uniqueness together, without copying the ranker's formula.
func TestToolSearchProperties(t *testing.T) {
	catalog, err := newToolSearchCatalog(discoveryOptions(t))
	require.NoError(t, err)
	names := make([]string, 0, len(catalog.tools))
	for name := range catalog.tools {
		names = append(names, name)
	}
	slices.Sort(names)
	rapid.Check(t, func(rt *rapid.T) {
		query := rapid.OneOf(rapid.SampledFrom(names), rapid.Map(rapid.SliceOfN(rapid.SampledFrom([]string{
			"issue", "comment", "deadline", "workflow", "cron", "read", "create", "猫", "___", "xyzzynotatool",
		}), 1, 12), func(words []string) string { return strings.Join(words, " ") })).Draw(rt, "query")
		limit := rapid.IntRange(1, 10).Draw(rt, "limit")
		input := ToolSearchInput{Query: query, Limit: &limit}
		require.NoError(rt, validateToolSearch(input))
		got := catalog.search(input)
		require.Equal(rt, got, catalog.search(input))
		require.LessOrEqual(rt, len(got.Results), limit)
		require.LessOrEqual(rt, len(mustJSON(t, got)), 32*1024)
		seen := make(map[string]bool)
		for _, result := range got.Results {
			require.Contains(rt, catalog.tools, result.Name)
			require.False(rt, seen[result.Name])
			seen[result.Name] = true
			require.NotContains(rt, result.Name, "kata.token_")
			require.NotContains(rt, result.Name, "kata.storage_")
		}
		if _, exact := catalog.tools[query]; exact {
			require.NotEmpty(rt, got.Results)
			require.Equal(rt, query, got.Results[0].Name)
		}
	})
}

func TestToolSearchExactNameHonorsLimit(t *testing.T) {
	// Named regression from Rapid seed 5173484949553690092: the exact-name
	// candidate already fills a one-result response before fuzzy ranking.
	session := connectRawTestServerWithOptions(t, discoveryOptions(t))
	output := searchToolSession(t, session, "kata.assign", 1)
	require.Len(t, output.Results, 1)
	require.Equal(t, "kata.assign", output.Results[0].Name)
}

func TestToolSearchOutputBudget(t *testing.T) {
	catalog := &toolSearchCatalog{tools: map[string]ToolSearchResult{
		"kata.comment": {Name: "kata.comment", Summary: strings.Repeat("comment ", 5000)},
	}, titles: map[string]string{}}
	output := catalog.search(ToolSearchInput{Query: "kata.comment"})
	require.LessOrEqual(t, len(mustJSON(t, output)), 32*1024)
	require.True(t, output.Truncated)
	require.Empty(t, output.Results)
	// The SDK emits JSON-v1-compatible escaping. Bound the actual encoded
	// response even when a description contains HTML-sensitive characters.
	for _, summary := range []string{strings.Repeat("<", 7000), strings.Repeat("\u2028", 6000)} {
		catalog.tools["kata.comment"] = ToolSearchResult{Name: "kata.comment", Summary: summary}
		output = catalog.search(ToolSearchInput{Query: "kata.comment"})
		require.LessOrEqual(t, len(mustJSON(t, output)), 32*1024)
		require.True(t, output.Truncated)
		require.Empty(t, output.Results)
	}
	catalog.tools = map[string]ToolSearchResult{
		"kata.comment": {Name: "kata.comment", Summary: "issue " + strings.Repeat("x", 20_000)},
		"kata.claim":   {Name: "kata.claim", Summary: "issue " + strings.Repeat("x", 20_000)},
	}
	output = catalog.search(ToolSearchInput{Query: "issue"})
	require.LessOrEqual(t, len(mustJSON(t, output)), 32*1024)
	require.True(t, output.Truncated)
	require.Len(t, output.Results, 1)
}

func TestToolSearchRejectsInvalidUTF8AndByteOverflow(t *testing.T) {
	for _, query := range []string{string([]byte{0xff}), strings.Repeat("猫", 171)} {
		require.Error(t, validateToolSearch(ToolSearchInput{Query: query}))
	}
	require.NoError(t, validateToolSearch(ToolSearchInput{Query: strings.Repeat("猫", 170) + "ab"}))
}

// The original arguments must be checked before accepting a string repaired
// by the SDK's permissive JSON decoder. A literal replacement rune is valid.
func TestToolSearchRejectsMalformedUnicodeOnWire(t *testing.T) {
	session := connectRawTestServerWithOptions(t, discoveryOptions(t))
	for _, query := range []string{`comment\ud800`, `comment\udfff`, "comment\xff"} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
			Name: "kata.search_tools", Arguments: json.RawMessage(`{"query":"` + query + `"}`),
		})
		require.NoError(t, err)
		require.True(t, result.IsError, "%q was silently repaired", query)
		require.Nil(t, result.StructuredContent)
	}
	for _, query := range []string{"comment�", `comment\ufffd`, `comment\ud83d\ude00`} {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
			Name: "kata.search_tools", Arguments: json.RawMessage(`{"query":"` + query + `"}`),
		})
		require.NoError(t, err)
		require.False(t, result.IsError, "%s", mustJSON(t, result))
	}
}

func TestToolSearchConcurrentSectionLoading(t *testing.T) {
	options := discoveryOptions(t)
	baseline := connectRawTestServerWithOptions(t, options)
	want := searchToolSession(t, baseline, "add a comment to an issue", 3)
	sections := []string{"issue_discovery", "issue_mutation", "coordination"}
	options.StartupSections = sections
	loadedBaseline := connectRawTestServerWithOptions(t, options)
	expected, err := loadedBaseline.ListTools(t.Context(), nil)
	require.NoError(t, err)
	options.StartupSections = nil
	session := connectRawTestServerWithOptions(t, options)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range 6 {
		group.Go(func() {
			<-start
			require.Equal(t, want, searchToolSession(t, session, "add a comment to an issue", 3))
		})
	}
	for _, section := range sections {
		group.Go(func() {
			<-start
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.load_" + section})
			require.NoError(t, err)
			require.False(t, result.IsError)
		})
	}
	close(start)
	group.Wait()
	// This session has not listed tools yet, so its first read observes the
	// final catalog without depending on the SDK's debounced notifications.
	actual, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, expected.Tools, actual.Tools, "only explicitly loaded sections may enter the public catalog")
}

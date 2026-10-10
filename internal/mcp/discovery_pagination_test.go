package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPDiscoveryPagination(t *testing.T) {
	env := testenv.New(t)
	p, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	foreign, err := env.DB.CreateProject(t.Context(), "foreign-project")
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := make([]db.ImportItem, 6)
	for i := range items {
		items[i] = db.ImportItem{ExternalID: fmt.Sprint(i), Title: "matching needle", Author: "example-agent", Status: "open", CreatedAt: base, UpdatedAt: base}
		if i > 0 {
			items[i].Priority = new(int64(i - 1))
		}
	}
	_, _, err = env.DB.ImportBatch(t.Context(), db.ImportBatchParams{ProjectID: p.ID, Source: "fixture", Actor: "example-agent", Items: items})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "other needle", Author: "example-agent"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: foreign.ID, Title: "foreign needle", Author: "example-agent"})
	require.NoError(t, err)
	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	bound, err := NewBoundScope(ProjectIdentity{ID: p.ID, UID: p.UID, Name: p.Name})
	require.NoError(t, err)
	allowed, err := NewAllowlistScope([]ProjectIdentity{{ID: p.ID, UID: p.UID, Name: p.Name}, {ID: other.ID, UID: other.UID, Name: other.Name}})
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		scope *Scope
		count int
	}{{"bound", bound, 6}, {"allowlist", allowed, 7}, {"all", NewAllScope(), 8}} {
		t.Run(tc.name, func(t *testing.T) {
			session := connectTestServerWithOptions(t, Options{Client: client, Scope: tc.scope, Actor: "example-agent", Version: "test"})
			cursor := ""
			seen := map[string]bool{}
			for i := 0; i < tc.count; i++ {
				args := map[string]any{"limit": 1, "include_total": true, "cursor": cursor}
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.list", Arguments: args})
				require.NoError(t, err)
				require.False(t, result.IsError, "%s", mustJSON(t, result))
				var page struct {
					Issues   []IssueSummary `json:"issues"`
					Next     string         `json:"next_cursor"`
					Complete bool           `json:"complete"`
					Total    int            `json:"total"`
				}
				require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &page))
				require.Len(t, page.Issues, 1)
				require.Equal(t, tc.count, page.Total)
				require.False(t, seen[page.Issues[0].UID])
				seen[page.Issues[0].UID] = true
				require.Equal(t, i == tc.count-1, page.Complete)
				if i < tc.count-1 {
					require.NotEmpty(t, page.Next)
				}
				cursor = page.Next
				rows, err := env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: p.ID})
				require.NoError(t, err)
				for _, row := range rows {
					_, _, _, err = env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: row.ID, Body: new(fmt.Sprintf("updated needle %d", i)), Actor: "example-agent"})
					require.NoError(t, err)
				}
			}
			require.Len(t, seen, tc.count)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"priority_unset": true, "project": p.Name, "include_total": true}})
			require.NoError(t, err)
			require.False(t, result.IsError, "%s", mustJSON(t, result))
			require.Equal(t, float64(1), result.StructuredContent.(map[string]any)["total"])
			result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"priority_unset": true, "priority": 0}})
			require.NoError(t, err)
			require.True(t, result.IsError)
		})
	}
	session := connectTestServerWithOptions(t, Options{Client: client, Scope: bound, Actor: "example-agent", Version: "test"})
	cursor := ""
	seen := map[string]bool{}
	for i := range 6 {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.search", Arguments: map[string]any{"query": "needle", "mode": "lexical", "limit": 1, "cursor": cursor}})
		require.NoError(t, err)
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		page := result.StructuredContent.(map[string]any)
		hits := page["results"].([]any)
		require.Len(t, hits, 1)
		uid := hits[0].(map[string]any)["issue"].(map[string]any)["uid"].(string)
		require.False(t, seen[uid])
		seen[uid] = true
		require.Equal(t, i == 5, page["complete"])
		cursor, _ = page["next_cursor"].(string)
		rows, err := env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: p.ID})
		require.NoError(t, err)
		for _, row := range rows {
			_, _, changed, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: row.ID, Body: new(fmt.Sprintf("search needle update %d", i)), Actor: "example-agent"})
			require.NoError(t, err)
			require.True(t, changed)
		}
	}
}

func TestMCPDiscoveryRejectsOldDaemon(t *testing.T) {
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/health", r.URL.Path)
		writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.26.0"})
	})
	handlers := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}
	_, _, err := handlers.list(t.Context(), nil, ListInput{})
	require.ErrorContains(t, err, "requires daemon API 0.27.0")
}

func TestMCPListSchemaPaginationConstraints(t *testing.T) {
	schema, err := inputSchemaFor[ListInput]("kata.list").Resolve(nil)
	require.NoError(t, err)
	require.NoError(t, schema.Validate(map[string]any{"sort": "oldest", "priority_unset": true}))
	require.Error(t, schema.Validate(map[string]any{"sort": "updated"}))
	require.Error(t, schema.Validate(map[string]any{"priority_unset": true, "priority": 0}))
	require.Error(t, schema.Validate(map[string]any{"priority_unset": true, "max_priority": 4}))
	require.NoError(t, schema.Validate(map[string]any{"priority_unset": false, "max_priority": 4}))
}

func TestMCPDiscoveryRechecksDaemonVersion(t *testing.T) {
	var version atomic.Int64
	version.Store(27)
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writeJSON(w, map[string]any{"ok": true, "api_schema_version": fmt.Sprintf("0.%d.0", version.Load())})
			return
		}
		if r.URL.Path == "/api/v1/projects" {
			writeJSON(w, map[string]any{"projects": []any{projectJSON(42, "01HAAAAAAAAAAAAAAAAAAAAAAA", "example-project")}})
			return
		}
		require.Equal(t, "/api/v1/projects/42/issues", r.URL.Path)
		writeJSON(w, map[string]any{"issues": []any{}, "complete": true})
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 42, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "example-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	_, _, err = handlers.list(t.Context(), nil, ListInput{})
	require.NoError(t, err)
	version.Store(26)
	_, _, err = handlers.list(t.Context(), nil, ListInput{})
	require.ErrorContains(t, err, "requires daemon API 0.27.0")
}

// Older supported daemons omit page metadata; their bounded search contract
// uses the extra-row probe rather than the zero value of an absent bool.
func TestMCPSearchCompletenessAcrossDaemonVersions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		version       string
		complete      *bool
		rows          int
		wantTruncated bool
	}{
		{"legacy before transcripts", "0.25.0", nil, 1, false},
		{"legacy exhausted", "0.26.0", nil, 1, false},
		{"legacy probe", "0.26.0", nil, 4, true},
		{"current exhausted", "0.27.0", new(true), 1, false},
		{"current incomplete", "0.27.0", new(false), 1, true},
	} {
		for _, mode := range []string{"auto", "hybrid", "semantic", "lexical"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				projects := []ProjectIdentity{{ID: 42, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "example-project"}}
				if mode == "lexical" {
					projects = append(projects, ProjectIdentity{ID: 43, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "other-project"})
				}
				client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/health":
						writeJSON(w, map[string]any{"ok": true, "api_schema_version": tc.version})
					case "/api/v1/projects":
						catalog := make([]any, 0, len(projects))
						for _, project := range projects {
							catalog = append(catalog, projectJSON(project.ID, project.UID, project.Name))
						}
						writeJSON(w, map[string]any{"projects": catalog})
					default:
						require.Contains(t, []string{"/api/v1/projects/42/search", "/api/v1/projects/43/search"}, r.URL.Path)
						results := make([]any, 0, tc.rows)
						for i := 0; i < tc.rows; i++ {
							results = append(results, map[string]any{"issue": issueJSON(42, "example-project", fmt.Sprintf("a%03d", i)), "score": 1.0, "matched_in": []string{"title"}})
						}
						body := map[string]any{"query": "needle", "mode": mode, "results": results}
						if tc.complete != nil {
							body["complete"] = *tc.complete
						}
						writeJSON(w, body)
					}
				})
				scope, err := NewAllowlistScope(projects)
				require.NoError(t, err)
				handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
				_, output, err := handlers.search(t.Context(), nil, SearchInput{Query: "needle", Mode: mode, Limit: 3})
				require.NoError(t, err)
				require.Equal(t, tc.wantTruncated, output.Truncated)
				require.Equal(t, !tc.wantTruncated, output.Complete)
			})
		}
	}
}

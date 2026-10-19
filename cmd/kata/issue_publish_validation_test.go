package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
)

type publicationValidationTransport func(*http.Request) (*http.Response, error)

func (f publicationValidationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type publicationValidationResolver struct{}

func (publicationValidationResolver) Transport(_ context.Context, _ githubsync.Binding, base http.RoundTripper) (http.RoundTripper, error) {
	return base, nil
}

// Exercise actual provider preflights through the daemon and CLI: locally known
// validation reasons must survive, while arbitrary upstream diagnostics must not.
func TestIssuePublishActionableValidation(t *testing.T) {
	const source = "11111111-1111-4111-8111-111111111111"
	const database = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const workspace = "22222222-2222-4222-8222-222222222222"
	const object = "55555555-5555-4555-8555-555555555552"
	const secret = "private-diagnostic-token-and-payload"
	for _, tc := range []struct{ name, provider, code, message, hint string }{
		{"notion-open-target", "notion", "publication_notion_open_target_required", "Notion publication requires an open status target.", "--todo-group"},
		{"twenty-required-field", "twenty", "publication_twenty_required_field", "Twenty requires an additional task field before publication.", "optional or give it a default"},
		{"github-title", "github", "publication_github_content_invalid", "GitHub publication content is invalid or exceeds its limits.", "256"},
		{"notion-binding", "notion", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"plane-title", "plane", "publication_plane_content_invalid", "Plane publication content is invalid or exceeds its limits.", "255"},
		{"plane-body", "plane", "publication_plane_content_invalid", "Plane publication content is invalid or exceeds its limits.", "escaped HTML"},
		{"linear-body", "linear", "publication_linear_content_invalid", "Linear publication content is invalid or exceeds its limits.", "1 MiB"},
		{"ticktick-body", "ticktick", "publication_content_invalid", "Publication content is invalid or exceeds its limits.", "1 MiB"},
		{"todoist-body", "todoist", "publication_content_invalid", "Publication content is invalid or exceeds its limits.", "1 MiB"},
		{"twenty-body", "twenty", "publication_content_invalid", "Publication content is invalid or exceeds its limits.", "1 MiB"},
		{"notion-title", "notion", "publication_notion_content_invalid", "Notion publication content is invalid or exceeds its limits.", "200000"},
		{"notion-body", "notion", "publication_notion_content_invalid", "Notion publication content is invalid or exceeds its limits.", "500 KiB"},
		{"github-archived", "github", "publication_destination_unavailable", "The publication destination is unavailable or does not match the binding.", "active destination"},
		{"ticktick-permission", "ticktick", "publication_write_permission_required", "The publication destination requires write permission.", "create/write access"},
		{"github-binding", "github", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"linear-binding", "linear", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"plane-binding", "plane", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"ticktick-binding", "ticktick", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"todoist-binding", "todoist", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"twenty-binding", "twenty", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"linear-workflow", "linear", "publication_open_target_required", "The publication destination has no valid open status target.", "open status"},
		{"plane-workflow", "plane", "publication_open_target_required", "The publication destination has no valid open status target.", "open status"},
		{"notion-schema", "notion", "publication_schema_invalid", "The publication destination schema does not support the configured fields or statuses.", "field and status mappings"},
		{"twenty-schema", "twenty", "publication_schema_invalid", "The publication destination schema does not support the configured fields or statuses.", "field and status mappings"},
		{"linear-destination", "linear", "publication_destination_unavailable", "The publication destination is unavailable or does not match the binding.", "active destination"},
		{"plane-destination", "plane", "publication_destination_unavailable", "The publication destination is unavailable or does not match the binding.", "active destination"},
		{"todoist-destination", "todoist", "publication_destination_unavailable", "The publication destination is unavailable or does not match the binding.", "active destination"},
		{"twenty-destination", "twenty", "publication_destination_unavailable", "The publication destination is unavailable or does not match the binding.", "active destination"},
		{"ticktick-destination", "ticktick", "publication_destination_unavailable", "The publication destination is unavailable or does not match the binding.", "active destination"},
		{"ticktick-two-way-permission", "ticktick", "publication_write_permission_required", "The publication destination requires write permission.", "create/write access"},
		{"linear-http", "linear", "publication_preflight_failed", "publication preflight failed", "Check the binding"},
		{"todoist-http", "todoist", "publication_source_unavailable", "publication source unavailable", "Check the binding"},
		{"plane-origin", "plane", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"todoist-origin", "todoist", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"twenty-origin", "twenty", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"todoist-account", "todoist", "publication_binding_invalid", "The publication binding configuration is invalid.", "enable"},
		{"linear-empty-schema", "linear", "publication_schema_invalid", "The publication destination schema does not support the configured fields or statuses.", "field and status mappings"},
		{"plane-empty-schema", "plane", "publication_schema_invalid", "The publication destination schema does not support the configured fields or statuses.", "field and status mappings"},
		{"twenty-empty-schema", "twenty", "publication_schema_invalid", "The publication destination schema does not support the configured fields or statuses.", "field and status mappings"},
		{"unsafe-http", "notion", "publication_preflight_failed", "publication preflight failed", "Check the binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads, creates atomic.Int32
			transport := publicationValidationTransport(func(r *http.Request) (*http.Response, error) {
				reads.Add(1)
				status := http.StatusOK
				var body any
				if strings.HasSuffix(tc.name, "-http") {
					status = http.StatusForbidden
					body = map[string]any{"message": secret, "code": "publication_notion_open_target_required"}
				} else if tc.provider == "linear" {
					var request struct {
						Query string `json:"query"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					if strings.Contains(request.Query, "KataLinearScope") {
						body = map[string]any{"data": map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": workspace}}, "team": map[string]any{"id": source, "name": secret, "organization": map[string]any{"id": workspace}}}}
					} else {
						require.Contains(t, request.Query, "KataLinearStates")
						body = map[string]any{"data": map[string]any{"workflowStates": map[string]any{"nodes": []any{map[string]any{"id": database, "type": "completed", "position": 0, "team": map[string]any{"id": source}}}, "pageInfo": map[string]any{"hasNextPage": false}}}}
					}
				} else if tc.provider == "plane" {
					if strings.HasSuffix(r.URL.Path, "/states/") {
						body = map[string]any{"results": []any{map[string]any{"id": database, "group": "completed", "sequence": 0}}, "next_page_results": false, "next_cursor": ""}
					} else {
						body = map[string]any{"id": source, "name": secret, "identifier": "EX"}
					}
				} else if tc.provider == "github" && r.URL.Path == "/repos/example-owner/example-repo" {
					body = map[string]any{"id": 123, "node_id": "R_example", "full_name": "example-owner/example-repo", "has_issues": true, "archived": true}
				} else if tc.provider == "ticktick" && r.URL.Path == "/open/v1/project/project-1" {
					body = map[string]any{"id": "project-1", "kind": "TASK", "permission": "read", "name": secret}
				} else if tc.provider == "todoist" && strings.HasPrefix(r.URL.Path, "/api/v1/projects/") {
					body = map[string]any{"id": "project-1", "name": secret, "is_archived": true, "is_deleted": false}
				} else if tc.provider == "todoist" && r.URL.Path == "/api/v1/user" {
					body = map[string]any{"id": "account-1"}
				} else if tc.provider == "notion" {
					switch r.URL.Path {
					case "/v1/data_sources/" + source:
						body = map[string]any{"object": "data_source", "id": source, "parent": map[string]any{"type": "database_id", "database_id": database}, "properties": map[string]any{
							"Title":    map[string]any{"id": "title", "type": "title", "title": map[string]any{}},
							"Status":   map[string]any{"id": "status", "type": "status", "status": map[string]any{"options": []any{map[string]any{"id": "ready", "name": "Ready"}, map[string]any{"id": "done", "name": "Done"}}, "groups": []any{map[string]any{"id": "todo", "name": "To-do", "option_ids": []string{"ready"}}, map[string]any{"id": "complete", "name": "Complete", "option_ids": []string{"done"}}}}},
							"Assignee": map[string]any{"id": "people", "type": "people", "people": map[string]any{}},
						}}
					case "/v1/databases/" + database:
						body = map[string]any{"object": "database", "id": database, "data_sources": []any{map[string]any{"id": source, "name": "Example tasks"}}}
					default:
						creates.Add(1)
						t.Errorf("unexpected Notion request %s", r.URL.Path)
					}
				} else if tc.provider == "twenty" && r.URL.Path == "/metadata" {
					var request struct {
						Query string `json:"query"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					connection := func(nodes []map[string]any) map[string]any {
						edges := []any{}
						for _, n := range nodes {
							edges = append(edges, map[string]any{"node": n})
						}
						return map[string]any{"edges": edges, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}
					}
					switch {
					case strings.Contains(request.Query, "KataTwentyWorkspace"):
						body = map[string]any{"data": map[string]any{"currentWorkspace": map[string]any{"id": workspace, "displayName": "Example workspace"}}}
					case strings.Contains(request.Query, "KataTwentyObjects"):
						body = map[string]any{"data": map[string]any{"objects": connection([]map[string]any{{"id": object, "nameSingular": "task", "isActive": true}})}}
					case strings.Contains(request.Query, "KataTwentyFields"):
						fields := []map[string]any{}
						for n, field := range []struct{ name, kind string }{{"title", "TEXT"}, {"bodyV2", "RICH_TEXT"}, {"status", "SELECT"}, {secret, "TEXT"}} {
							f := map[string]any{"id": fmt.Sprintf("44444444-4444-4444-8444-%012d", n+1), "name": field.name, "type": field.kind, "isActive": true, "isSystem": false, "isNullable": false, "defaultValue": nil}
							if tc.name == "twenty-schema" && field.name == "title" {
								f["type"] = "NUMBER"
							}
							if field.name == "status" {
								f["options"] = []any{map[string]any{"value": "TODO"}, map[string]any{"value": "IN_PROGRESS"}, map[string]any{"value": "DONE"}}
							}
							fields = append(fields, f)
						}
						body = map[string]any{"data": map[string]any{"object": map[string]any{"id": object, "nameSingular": "task", "isActive": true, "fields": connection(fields)}}}
					default:
						t.Errorf("unexpected metadata query")
					}
				} else {
					creates.Add(1)
					t.Errorf("unexpected provider request %s", r.URL.Path)
				}
				switch tc.name {
				case "linear-empty-schema":
					if value, ok := body.(map[string]any)["data"].(map[string]any)["workflowStates"]; ok {
						value.(map[string]any)["nodes"] = []any{}
					}
				case "plane-empty-schema":
					if strings.HasSuffix(r.URL.Path, "/states/") {
						body.(map[string]any)["results"] = []any{}
					}
				case "twenty-empty-schema":
					if value, ok := body.(map[string]any)["data"].(map[string]any)["objects"]; ok {
						value.(map[string]any)["edges"] = []any{}
					}
				case "todoist-account":
					body.(map[string]any)["id"] = "other-account"
				case "linear-destination":
					body.(map[string]any)["data"].(map[string]any)["team"].(map[string]any)["archivedAt"] = "2026-01-01T00:00:00Z"
				case "plane-destination":
					body.(map[string]any)["archived_at"] = "2026-01-01T00:00:00Z"
				case "ticktick-destination":
					body.(map[string]any)["closed"] = true
				case "twenty-destination":
					body.(map[string]any)["data"].(map[string]any)["currentWorkspace"].(map[string]any)["id"] = database
				}
				if tc.name == "notion-schema" && strings.Contains(r.URL.Path, "data_sources") {
					body.(map[string]any)["properties"].(map[string]any)["Title"].(map[string]any)["type"] = "rich_text"
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
			})
			lookup := func(string) (string, bool) { return secret, true }
			env := testenv.New(t, func(c *daemon.ServerConfig) {
				c.LinearSyncFetcher = linearsync.NewClient(linearsync.ClientConfig{LookupEnv: lookup, Transport: transport})
				c.PlaneSyncFetcher = planesync.NewClient(planesync.ClientConfig{Daemon: config.PlaneSyncConfig{APIOrigin: "https://plane.example"}, LookupEnv: lookup, Transport: transport})
				c.TickTickSyncFetcher = tickticksync.NewClient(tickticksync.ClientConfig{LookupEnv: lookup, Transport: transport})
				c.TodoistSyncFetcher = todoistsync.NewClient(todoistsync.ClientConfig{LookupEnv: lookup, Transport: transport})
				c.NotionSyncFetcher = notionsync.NewClient(notionsync.ClientConfig{LookupEnv: lookup, Transport: transport})
				c.TwentySyncFetcher = twentysync.NewClient(twentysync.ClientConfig{Daemon: config.TwentySyncConfig{APIOrigin: "https://twenty.example"}, LookupEnv: lookup, Transport: transport})
				c.GitHubSyncFetcher = githubsync.NewHTTPFetcher(githubsync.HTTPFetcherConfig{Client: &http.Client{Transport: transport}, CredentialResolver: publicationValidationResolver{}})
			})
			dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
			project, err := env.DB.ProjectByName(t.Context(), "spoke-project")
			require.NoError(t, err)
			title := "Native issue"
			body := ""
			switch tc.name {
			case "github-title":
				title = strings.Repeat("x", 257)
			case "plane-title":
				title = strings.Repeat("x", 256)
			case "notion-title":
				title = strings.Repeat("x", 200001)
			case "plane-body":
				body = strings.Repeat("&", (1<<20)/4)
			case "notion-body":
				body = strings.Repeat("x", 500*1024)
			case "linear-body", "ticktick-body", "todoist-body", "twenty-body":
				body = strings.Repeat("x", (1<<20)+1)
			}
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: title, Body: body, Author: "worker"})
			require.NoError(t, err)
			binding := db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: tc.provider, DisplayName: "Example source", IntervalSeconds: 300}
			switch tc.provider {
			case "linear":
				c := linearsync.Config{WorkspaceID: workspace, TeamID: source}
				binding.SourceKey = c.SourceKey()
				binding.RemoteID = c.RemoteID()
				binding.Config, err = linearsync.EncodeConfig(c)
			case "plane":
				c := planesync.Config{APIOrigin: "https://plane.example", WebOrigin: "https://plane.example", Workspace: "example-workspace", ProjectID: source}
				if tc.name == "plane-origin" {
					c.APIOrigin = "https://other.example"
				}
				binding.SourceKey = c.SourceKey()
				binding.RemoteID = c.RemoteID()
				binding.Config, err = planesync.EncodeConfig(c)
			case "ticktick":
				c := tickticksync.Config{ProjectID: "project-1"}
				if tc.name == "ticktick-two-way-permission" {
					c.StatusSync = "two-way"
				}
				binding.SourceKey = c.SourceKey()
				binding.RemoteID = c.RemoteID()
				binding.Config, err = tickticksync.EncodeConfig(c)
			case "todoist":
				c := todoistsync.Config{APIOrigin: "https://api.todoist.com", AccountID: "account-1", ProjectID: "project-1", HistorySince: "2026-01-01T00:00:00Z"}
				if tc.name == "todoist-origin" {
					c.APIOrigin = "http://127.0.0.1:9999"
				}
				binding.SourceKey = c.SourceKey()
				binding.RemoteID = c.RemoteID()
				binding.Config, err = todoistsync.EncodeConfig(c)
			case "notion":
				binding.SourceKey = "notion:" + source
				binding.RemoteID = source
				binding.Config, err = notionsync.EncodeConfig(notionsync.Config{DataSourceID: source, DatabaseID: database, TitlePropertyID: "title", StatusPropertyID: "status", AssigneePropertyID: "people", DoneStatusIDs: []string{"done"}})
			case "twenty":
				c := twentysync.Config{APIOrigin: "https://twenty.example", WebOrigin: "https://twenty.example", WorkspaceID: workspace}
				if tc.name == "twenty-origin" {
					c.APIOrigin = "https://other.example"
				}
				binding.SourceKey = c.SourceKey()
				binding.RemoteID = c.RemoteID()
				binding.Config, err = twentysync.EncodeConfig(c)
			case "github":
				binding.SourceKey = "github:R_example"
				binding.RemoteID = "R_example"
				binding.Config, err = githubsync.EncodeConfig(githubsync.Config{Host: "github.com", Owner: "example-owner", Repo: "example-repo", RepoID: 123})
			}
			require.NoError(t, err)
			if tc.name == "notion-title" || tc.name == "notion-body" {
				binding.Config, err = notionsync.EncodeConfig(notionsync.Config{DataSourceID: source, DatabaseID: database, TitlePropertyID: "title", StatusPropertyID: "status", AssigneePropertyID: "people", CompleteGroupID: "complete", TodoGroupID: "todo"})
				require.NoError(t, err)
			}
			if strings.HasSuffix(tc.name, "-binding") {
				binding.Config = []byte(`{}`)
			}
			_, err = env.DB.UpsertIssueSyncBinding(t.Context(), binding)
			require.NoError(t, err)
			resp, err := env.HTTP.Post(fmt.Sprintf("%s/api/v1/projects/%d/issue-sync/%s/publish", env.URL, project.ID, tc.provider), "application/json", strings.NewReader(fmt.Sprintf(`{"issue_ref":%q,"actor":"worker"}`, issue.ShortID)))
			require.NoError(t, err)
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Contains(t, string(raw), tc.code)
			require.Contains(t, string(raw), tc.message)
			require.Contains(t, string(raw), tc.hint)
			require.NotContains(t, string(raw), secret)
			if !strings.HasSuffix(tc.name, "-http") {
				require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			}
			_, _, err = runCLIWithErr(t, env, dir, "sync", tc.provider, "publish", issue.ShortID)
			cli, ok := errors.AsType[*cliError](err)
			require.True(t, ok, "%v", err)
			require.Equal(t, tc.code, cli.Code)
			require.Equal(t, tc.message, cli.Message)
			require.Contains(t, cli.Hint, tc.hint)
			for _, mode := range []outputMode{outputHuman, outputJSON, outputAgent} {
				var rendered bytes.Buffer
				emitErrorForMode(&rendered, err, mode, true)
				require.Contains(t, rendered.String(), tc.message)
				require.Contains(t, rendered.String(), tc.hint)
				require.NotContains(t, rendered.String(), secret)
			}
			pending, pendingErr := env.DB.PendingIssuePublication(t.Context(), project.ID)
			require.NoError(t, pendingErr)
			require.Nil(t, pending)
			require.Zero(t, creates.Load())
			if tc.name == "github-title" || strings.HasSuffix(tc.name, "-binding") || strings.HasSuffix(tc.name, "-origin") || tc.name == "plane-title" || tc.name == "plane-body" || tc.name == "linear-body" || tc.name == "ticktick-body" || tc.name == "twenty-body" {
				require.Zero(t, reads.Load())
			} else {
				require.Positive(t, reads.Load())
			}
		})
	}
}

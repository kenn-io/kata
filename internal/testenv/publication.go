package testenv

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
)

const publicationSource = "11111111-1111-4111-8111-111111111111"
const publicationDatabase = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const publicationWorkspace = "22222222-2222-4222-8222-222222222222"
const publicationItem = "33333333-3333-4333-8333-333333333333"
const publicationState = "44444444-4444-4444-8444-444444444444"
const publicationObject = "55555555-5555-4555-8555-555555555555"

// PublicationUpstream exercises real startup-created provider clients without
// external network access. Install only in nonparallel tests, before startup.
type PublicationUpstream struct {
	mu      sync.Mutex
	creates map[string]int
	local   http.RoundTripper
}

// InstallPublicationUpstream installs bounded provider responses and fixture
// credentials. Loopback daemon calls use the original transport.
func InstallPublicationUpstream(t *testing.T) *PublicationUpstream {
	t.Helper()
	f := &PublicationUpstream{creates: map[string]int{}, local: http.DefaultTransport}
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = f.local })
	for _, provider := range []string{"GITHUB", "NOTION", "LINEAR", "PLANE", "TICKTICK", "TODOIST", "TWENTY"} {
		t.Setenv("KATA_"+provider+"_TOKEN", "fixture-token")
	}
	return f
}

// Creates returns the number of actual upstream mutation requests.
func (f *PublicationUpstream) Creates(provider string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates[provider]
}

// Binding returns a valid source configuration for the fixture provider.
func (f *PublicationUpstream) Binding(t *testing.T, projectID int64, provider string) db.UpsertIssueSyncBindingParams {
	t.Helper()
	b := db.UpsertIssueSyncBindingParams{ProjectID: projectID, Provider: provider, DisplayName: "Example source", IntervalSeconds: 3600}
	var err error
	switch provider {
	case "github":
		b.SourceKey = "github:R_example"
		b.RemoteID = "R_example"
		b.Config, err = githubsync.EncodeConfig(githubsync.Config{Host: "github.com", Owner: "example-owner", Repo: "example-repo", RepoID: 123})
	case "notion":
		b.SourceKey = "notion:" + publicationSource
		b.RemoteID = publicationSource
		b.Config, err = notionsync.EncodeConfig(notionsync.Config{DataSourceID: publicationSource, DatabaseID: publicationDatabase, TitlePropertyID: "title", StatusPropertyID: "status", AssigneePropertyID: "people", CompleteGroupID: "complete", TodoGroupID: "todo"})
	case "linear":
		c := linearsync.Config{WorkspaceID: publicationWorkspace, TeamID: publicationSource}
		b.SourceKey = c.SourceKey()
		b.RemoteID = c.RemoteID()
		b.Config, err = linearsync.EncodeConfig(c)
	case "plane":
		c := planesync.Config{APIOrigin: "https://api.plane.so", WebOrigin: "https://app.plane.so", Workspace: "example-workspace", ProjectID: publicationSource}
		b.SourceKey = c.SourceKey()
		b.RemoteID = c.RemoteID()
		b.Config, err = planesync.EncodeConfig(c)
	case "ticktick":
		c := tickticksync.Config{ProjectID: "project-1"}
		b.SourceKey = c.SourceKey()
		b.RemoteID = c.RemoteID()
		b.Config, err = tickticksync.EncodeConfig(c)
	case "todoist":
		c := todoistsync.Config{APIOrigin: "https://api.todoist.com", AccountID: "account-1", ProjectID: "project-1", HistorySince: "2026-01-01T00:00:00Z"}
		b.SourceKey = c.SourceKey()
		b.RemoteID = c.RemoteID()
		b.Config, err = todoistsync.EncodeConfig(c)
	case "twenty":
		c := twentysync.Config{APIOrigin: "https://api.twenty.com", WebOrigin: "https://app.twenty.com", WorkspaceID: publicationWorkspace}
		b.SourceKey = c.SourceKey()
		b.RemoteID = c.RemoteID()
		b.Config, err = twentysync.EncodeConfig(c)
	default:
		t.Fatalf("unsupported provider %q", provider)
	}
	require.NoError(t, err)
	return b
}

// RoundTrip serves only known provider requests; unexpected external requests fail.
func (f *PublicationUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(r.URL.Hostname()); ip != nil && ip.IsLoopback() {
		return f.local.RoundTrip(r)
	}
	provider := map[string]string{"api.github.com": "github", "api.notion.com": "notion", "api.linear.app": "linear", "api.plane.so": "plane", "api.ticktick.com": "ticktick", "api.todoist.com": "todoist", "api.twenty.com": "twenty"}[r.URL.Hostname()]
	if provider == "" {
		return nil, fmt.Errorf("unexpected fixture origin %s", r.URL.Host)
	}
	path := r.URL.Path
	var request map[string]any
	if r.Body != nil {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
		}
	}
	query, _ := request["query"].(string)
	data := func(value any) any { return map[string]any{"data": value} }
	create := false
	var body any
	switch provider {
	case "github":
		if path == "/repos/example-owner/example-repo" {
			body = map[string]any{"id": 123, "node_id": "R_example", "full_name": "example-owner/example-repo", "has_issues": true, "archived": false}
		} else if strings.HasPrefix(path, "/repos/example-owner/example-repo/issues") {
			create = r.Method == "POST"
			body = map[string]any{"id": 456, "number": 7, "url": "https://api.github.com/repos/example-owner/example-repo/issues/7", "html_url": "https://github.com/example-owner/example-repo/issues/7", "state": "open", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}
		}
	case "notion":
		props := map[string]any{"Title": map[string]any{"id": "title", "type": "title", "title": map[string]any{}}, "Status": map[string]any{"id": "status", "type": "status", "status": map[string]any{"options": []any{map[string]any{"id": "ready", "name": "Ready"}, map[string]any{"id": "done", "name": "Done"}}, "groups": []any{map[string]any{"id": "todo", "name": "To-do", "option_ids": []string{"ready"}}, map[string]any{"id": "complete", "name": "Complete", "option_ids": []string{"done"}}}}}, "Assignee": map[string]any{"id": "people", "type": "people", "people": map[string]any{}}}
		switch path {
		case "/v1/data_sources/" + publicationSource:
			body = map[string]any{"object": "data_source", "id": publicationSource, "parent": map[string]any{"type": "database_id", "database_id": publicationDatabase}, "properties": props}
		case "/v1/databases/" + publicationDatabase:
			body = map[string]any{"object": "database", "id": publicationDatabase, "data_sources": []any{map[string]any{"id": publicationSource, "name": "Example"}}}
		case "/v1/pages", "/v1/pages/" + publicationItem:
			create = r.Method == "POST"
			props["Title"] = map[string]any{"id": "title", "type": "title", "title": []any{}}
			props["Status"] = map[string]any{"id": "status", "type": "status", "status": map[string]any{"id": "ready"}}
			props["Assignee"] = map[string]any{"id": "people", "type": "people", "people": []any{}}
			body = map[string]any{"object": "page", "id": publicationItem, "url": "https://www.notion.so/" + publicationItem, "is_archived": false, "in_trash": false, "parent": map[string]any{"type": "data_source_id", "data_source_id": publicationSource, "database_id": publicationDatabase}, "created_time": "2026-01-01T00:00:00Z", "last_edited_time": "2026-01-02T00:00:00Z", "properties": props}
		}
	case "linear":
		issue := map[string]any{"id": publicationItem, "team": map[string]any{"id": publicationSource}, "project": nil, "archivedAt": nil, "trashed": nil, "completedAt": nil, "canceledAt": nil, "state": map[string]any{"id": publicationState}, "identifier": "EX-1", "title": "Native issue", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z"}
		switch {
		case strings.Contains(query, "KataLinearScope"):
			body = data(map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": publicationWorkspace}}, "team": map[string]any{"id": publicationSource, "name": "Example", "organization": map[string]any{"id": publicationWorkspace}}})
		case strings.Contains(query, "KataLinearStates"):
			body = data(map[string]any{"workflowStates": map[string]any{"nodes": []any{map[string]any{"id": publicationState, "type": "unstarted", "position": 0, "team": map[string]any{"id": publicationSource}}}, "pageInfo": map[string]any{"hasNextPage": false}}})
		case strings.Contains(query, "KataLinearCreate"):
			create = true
			body = data(map[string]any{"issueCreate": map[string]any{"success": true, "issue": issue}})
		case strings.Contains(query, "KataLinearPublished"):
			body = data(map[string]any{"issue": issue})
		}
	case "plane":
		prefix := "/api/v1/workspaces/example-workspace/projects/" + publicationSource + "/"
		if path == prefix {
			body = map[string]any{"id": publicationSource, "name": "Example", "identifier": "EX"}
		} else if path == prefix+"states/" {
			body = map[string]any{"results": []any{map[string]any{"id": publicationState, "group": "unstarted", "sequence": 0}}, "next_page_results": false}
		} else if strings.HasPrefix(path, prefix+"work-items/") {
			create = r.Method == "POST"
			body = map[string]any{"id": publicationItem, "project": publicationSource, "state": publicationState, "sequence_id": 42, "name": "Native issue", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}
		}
	case "ticktick":
		switch path {
		case "/open/v1/project/project-1":
			body = map[string]any{"id": "project-1", "kind": "TASK", "permission": "write"}
		case "/open/v1/task", "/open/v1/project/project-1/task/task-1":
			create = r.Method == "POST"
			body = map[string]any{"id": "task-1", "projectId": "project-1", "status": 0, "kind": "TEXT"}
		}
	case "todoist":
		switch path {
		case "/api/v1/user":
			body = map[string]any{"id": "account-1"}
		case "/api/v1/projects/project-1":
			body = map[string]any{"id": "project-1", "name": "Example", "is_archived": false, "is_deleted": false}
		case "/api/v1/tasks", "/api/v1/tasks/task-1":
			create = r.Method == "POST"
			body = map[string]any{"id": "task-1", "project_id": "project-1", "content": "Native issue", "description": "", "checked": false, "is_deleted": false, "priority": 1, "added_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z", "labels": []any{}}
		}
	case "twenty":
		connection := func(nodes []any) any {
			edges := []any{}
			for _, n := range nodes {
				edges = append(edges, map[string]any{"node": n})
			}
			return map[string]any{"edges": edges, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}
		}
		switch {
		case strings.Contains(query, "KataTwentyWorkspace"):
			body = data(map[string]any{"currentWorkspace": map[string]any{"id": publicationWorkspace, "displayName": "Example"}})
		case strings.Contains(query, "KataTwentyObjects"):
			body = data(map[string]any{"objects": connection([]any{map[string]any{"id": publicationObject, "nameSingular": "task", "isActive": true}})})
		case strings.Contains(query, "KataTwentyFields"):
			fields := []any{}
			for n, field := range []struct{ name, kind string }{{"title", "TEXT"}, {"bodyV2", "RICH_TEXT"}, {"status", "SELECT"}} {
				f := map[string]any{"id": fmt.Sprintf("44444444-4444-4444-8444-%012d", n+1), "name": field.name, "type": field.kind, "isActive": true, "isSystem": false, "isNullable": true, "defaultValue": nil}
				if field.name == "status" {
					f["options"] = []any{map[string]any{"value": "TODO"}, map[string]any{"value": "IN_PROGRESS"}, map[string]any{"value": "DONE"}}
				}
				fields = append(fields, f)
			}
			body = data(map[string]any{"object": map[string]any{"id": publicationObject, "nameSingular": "task", "isActive": true, "fields": connection(fields)}})
		case path == "/rest/tasks" && r.Method == "POST":
			create = true
			body = data(map[string]any{"createTask": map[string]any{"id": publicationItem}})
		case path == "/rest/tasks/"+publicationItem:
			body = data(map[string]any{"task": map[string]any{"id": publicationItem, "title": "Native issue", "bodyV2": map[string]any{"markdown": ""}, "status": "TODO", "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z", "deletedAt": nil}})
		}
	}
	if body == nil {
		return nil, fmt.Errorf("unexpected %s request %s %s", provider, r.Method, path)
	}
	if create {
		f.mu.Lock()
		f.creates[provider]++
		f.mu.Unlock()
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
}

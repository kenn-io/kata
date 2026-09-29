package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/testenv"
)

const cliNotionSource = "11111111-1111-4111-8111-111111111111"
const cliNotionDatabase = "22222222-2222-4222-8222-222222222222"
const cliNotionPage = "33333333-3333-4333-8333-333333333333"

// The proxy keeps project resolution real and records the daemon API boundary.
func notionCLIHTTP(t *testing.T, handler http.HandlerFunc) (*testenv.Env, string) {
	t.Helper()
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issue-sync/") {
			handler(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return &testenv.Env{URL: server.URL}, dir
}

func cliNotionResponse() map[string]any {
	return map[string]any{
		"binding": map[string]any{"id": 1, "project_id": 1, "provider": "notion", "source_key": "notion:" + cliNotionSource, "remote_id": cliNotionSource, "display_name": "Tasks", "enabled": true, "interval_seconds": 300, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z", "config": map[string]any{"data_source_id": cliNotionSource, "database_id": cliNotionDatabase, "title_property_id": "title", "status_property_id": "state", "assignee_property_id": "people", "done_status_ids": []string{"done", "accepted"}, "since": "2026-01-01T00:00:00Z"}},
		"status":  map[string]any{"provider": "notion", "state": "enabled", "enabled": true, "last_created": 2, "last_updated": 3, "last_unchanged": 4},
		"import":  map[string]any{"source": "notion", "created": 2, "updated": 3, "unchanged": 4},
	}
}

func TestNotionSyncCommands(t *testing.T) {
	var paths []string
	env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/enable") {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "10m", body["interval"])
			require.Equal(t, map[string]any{"data_source_id": cliNotionSource, "status_property": "Workflow", "assignee_property": "Responsible", "done_statuses": []any{"Delivered", "Accepted"}}, body["config"])
		}
		require.NoError(t, json.NewEncoder(w).Encode(cliNotionResponse()))
	})
	out := runCLI(t, env, dir, "sync", "notion", "enable", "--data-source", cliNotionSource, "--status-property", "Workflow", "--assignee-property", "Responsible", "--done-status", "Delivered", "--done-status", "Accepted", "--interval", "10m")
	require.Contains(t, out, "Notion sync enabled")
	out = runCLI(t, env, dir, "--json", "sync", "notion", "status")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &decoded))
	require.Equal(t, "notion", decoded["binding"].(map[string]any)["provider"])
	require.Contains(t, runCLI(t, env, dir, "--agent", "sync", "notion", "once"), "OK notion-sync action=once")
	require.Contains(t, runCLI(t, env, dir, "sync", "notion", "disable"), "Notion sync disabled")
	require.Len(t, paths, 4)
	for i, action := range []string{"enable", "status", "once", "disable"} {
		method := "POST"
		if action == "status" {
			method = "GET"
		}
		require.Regexp(t, fmt.Sprintf(`^%s /api/v1/projects/\d+/issue-sync/notion/%s$`, method, action), paths[i])
	}
}

func TestNotionSincePresence(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		present     bool
		want        string
	}{{"omitted", "", false, ""}, {"empty", "", true, ""}, {"replacement", "2026-01-02T03:00:00+03:00", true, "2026-01-02T00:00:00Z"}} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Config map[string]any `json:"config"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				value, present := body.Config["since"]
				require.Equal(t, tc.present, present)
				if present {
					require.Equal(t, tc.want, value)
				}
				require.NoError(t, json.NewEncoder(w).Encode(cliNotionResponse()))
			})
			args := []string{"sync", "notion", "enable"}
			if tc.present {
				args = append(args, "--since", tc.value)
			}
			runCLI(t, env, dir, args...)
		})
	}
}

func TestNotionCLIIsDaemonOwned(t *testing.T) {
	t.Setenv("KATA_NOTION_TOKEN", "")
	env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Config map[string]any `json:"config"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, cliNotionDatabase, body.Config["database"])
		require.NoError(t, json.NewEncoder(w).Encode(cliNotionResponse()))
	})
	require.Contains(t, runCLI(t, env, dir, "sync", "notion", "enable", "--database", "https://www.notion.so/Tasks-22222222222242228222222222222222?v=ignored", "--done-status", "Delivered"), "Notion sync enabled")
}

func TestNotionSyncInvalidInputsRejectBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{
		{"--database", "https://daemon.example/Tasks-22222222222242228222222222222222"},
		{"--data-source", "https://www.notion.so/" + cliNotionSource},
		{"--data-source", cliNotionSource, "--database", cliNotionDatabase},
		{"--done-status", ""}, {"--status-property", ""}, {"--assignee-property", ""},
		{"--interval", "bad"}, {"--interval", "0s"}, {"--since", "bad"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			env, dir := notionCLIHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached daemon") })
			_, _, err := runCLIWithErr(t, env, dir, append([]string{"sync", "notion", "enable"}, args...)...)
			_ = requireCLIError(t, err, ExitValidation)
		})
	}
}

func TestNotionSyncDaemonAmbiguity(t *testing.T) {
	env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"validation_error","message":"select exactly one Notion status property; choices: Workflow (state), Review (review)"}}`)
	})
	_, stderr, err := runCLIWithErr(t, env, dir, "sync", "notion", "enable", "--data-source", cliNotionSource, "--done-status", "Delivered")
	require.Error(t, err)
	require.Contains(t, stderr, "Workflow (state)")
}

func TestNotionStatusModes(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		t.Run(mode, func(t *testing.T) {
			response := cliNotionResponse()
			binding := response["binding"].(map[string]any)
			binding["display_name"] = "Tasks\nInjected\x1b[31m"
			status := response["status"].(map[string]any)
			status["state"] = "running"
			status["last_error"] = "upstream\nfailed\x1b[31m"
			status["last_success_at"] = "2026-01-01T00:00:00Z"
			status["last_attempt_at"] = "2026-01-02T00:00:00Z"
			status["sync_started_at"] = "2026-01-02T00:00:00Z"
			status["progress"] = map[string]any{"phase": "content\nunsafe", "completed": 9, "total": 0, "started_at": "2026-01-02T00:00:00Z", "updated_at": "2026-01-02T00:01:00Z"}
			env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(response)) })
			args := []string{"sync", "notion", "status"}
			if mode != "human" {
				args = append([]string{"--" + mode}, args...)
			}
			out := runCLI(t, env, dir, args...)
			require.NotContains(t, out, "token_env")
			switch mode {
			case "human":
				for _, want := range []string{"Notion sync running", "total unknown", "Last successful run: created=2 updated=3 unchanged=4", "Last attempt:", "Started:", "Source:", "Database:", "Title property:", "Status property:", "Assignee property:", "Completed options:", "Since:"} {
					require.Contains(t, out, want)
				}
				require.NotContains(t, out, "\nInjected")
				require.NotContains(t, out, "\x1b")
			case "agent":
				require.NotContains(t, out, "\n")
				for _, want := range []string{"last_created=2", "phase=", "total=0", "data_source_id=", "done_status_ids=", "since=", "last_success_at="} {
					require.Contains(t, out, want)
				}
			default:
				var decoded map[string]any
				require.NoError(t, json.Unmarshal([]byte(out), &decoded))
				require.Equal(t, "Tasks\nInjected\x1b[31m", decoded["binding"].(map[string]any)["display_name"])
			}
		})
	}
	for _, state := range []string{"disabled", "not_enabled"} {
		t.Run(state, func(t *testing.T) {
			env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"provider": "notion", "state": state, "enabled": false}}))
			})
			out := runCLI(t, env, dir, "sync", "notion", "status")
			require.Contains(t, out, "Notion sync "+state)
			require.Contains(t, out, "No successful run yet")
		})
	}
}

type cliNotionTransport func(*http.Request) (*http.Response, error)

func (f cliNotionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This acceptance flow exercises the real CLI, daemon, Notion HTTP client,
// adapter, runner, and temporary SQLite store, with only upstream HTTP replaced.
func TestNotionSyncReadOnlyAcceptance(t *testing.T) {
	t.Setenv("KATA_NOTION_TOKEN", "")
	var completed atomic.Bool
	var sourceTitle atomic.Value
	sourceTitle.Store([]any{})
	var editedAt atomic.Value
	editedAt.Store(time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano))
	var requests, queries, mutations atomic.Int32
	token := "example-daemon-notion-token"
	transport := cliNotionTransport(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		require.Equal(t, "https://api.notion.com", r.URL.Scheme+"://"+r.URL.Host)
		require.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
		require.Equal(t, "2026-03-11", r.Header.Get("Notion-Version"))
		if r.Method != http.MethodGet {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/data_sources/"+cliNotionSource+"/query" {
				queries.Add(1)
			} else {
				mutations.Add(1)
				return nil, fmt.Errorf("unexpected Notion mutation")
			}
		}
		status, edited := "active", editedAt.Load().(string)
		if completed.Load() {
			status = "done"
		}
		page := map[string]any{"object": "page", "id": cliNotionPage, "url": "https://www.notion.so/" + cliNotionPage, "parent": map[string]any{"type": "data_source_id", "data_source_id": cliNotionSource, "database_id": cliNotionDatabase}, "created_time": "2026-01-01T00:00:00Z", "last_edited_time": edited, "is_archived": false, "in_trash": false, "properties": map[string]any{"Workflow": map[string]any{"id": "state", "type": "status", "status": map[string]any{"id": status}}}}
		var body any
		switch r.URL.Path {
		case "/v1/databases/" + cliNotionDatabase:
			body = map[string]any{"object": "database", "id": cliNotionDatabase, "data_sources": []any{map[string]any{"id": cliNotionSource, "name": "Tasks"}}}
		case "/v1/data_sources/" + cliNotionSource:
			body = map[string]any{"object": "data_source", "id": cliNotionSource, "parent": map[string]any{"type": "database_id", "database_id": cliNotionDatabase}, "title": sourceTitle.Load(), "properties": map[string]any{"Task": map[string]any{"id": "title", "type": "title"}, "Owner": map[string]any{"id": "people", "type": "people"}, "Workflow": map[string]any{"id": "state", "type": "status", "status": map[string]any{"options": []any{map[string]any{"id": "active", "name": "Doing"}, map[string]any{"id": "done", "name": "Delivered"}}}}}}
		case "/v1/data_sources/" + cliNotionSource + "/query":
			var query struct {
				Filter *struct {
					Timestamp string `json:"timestamp"`
					Edited    struct {
						OnOrAfter string `json:"on_or_after"`
					} `json:"last_edited_time"`
				} `json:"filter"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&query))
			if query.Filter != nil {
				require.Equal(t, "last_edited_time", query.Filter.Timestamp)
				cutoff, err := time.Parse(time.RFC3339Nano, query.Filter.Edited.OnOrAfter)
				require.NoError(t, err)
				changed, err := time.Parse(time.RFC3339Nano, edited)
				require.NoError(t, err)
				require.False(t, changed.Before(cutoff), "fake page must satisfy real incremental query cutoff")
			}
			body = map[string]any{"object": "list", "results": []any{page}, "has_more": false, "next_cursor": nil, "request_status": map[string]any{"type": "complete"}}
		case "/v1/pages/" + cliNotionPage:
			body = page
		case "/v1/pages/" + cliNotionPage + "/properties/title", "/v1/pages/" + cliNotionPage + "/properties/people":
			id, kind := "title", "title"
			values := []any{map[string]any{"object": "property_item", "id": "title", "type": "title", "title": map[string]any{"plain_text": "Imported task"}}}
			if strings.HasSuffix(r.URL.Path, "/people") {
				id, kind = "people", "people"
				values = []any{}
			}
			body = map[string]any{"object": "list", "type": "property_item", "results": values, "has_more": false, "next_cursor": nil, "property_item": map[string]any{"id": id, "type": kind}}
		case "/v1/pages/" + cliNotionPage + "/markdown":
			body = map[string]any{"object": "page_markdown", "id": cliNotionPage, "markdown": "Task body", "truncated": false, "unknown_block_ids": []any{}}
		default:
			return nil, fmt.Errorf("unexpected Notion read path %s", r.URL.Path)
		}
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
	})
	client := notionsync.NewClient(notionsync.ClientConfig{Transport: transport, LookupEnv: func(string) (string, bool) { return token, true }})
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.NotionSyncFetcher = client })
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	projectID := resolvePIDViaHTTP(t, env.URL, dir)
	// Enable must also wait for paced Notion reads beyond the ordinary CLI timeout.
	t.Setenv("KATA_HTTP_TIMEOUT", "100ms")
	enable := runCLI(t, env, dir, "sync", "notion", "enable", "--data-source", cliNotionSource, "--done-status", "Delivered")
	require.NotContains(t, enable, token)
	binding, err := env.DB.IssueSyncBindingByProject(t.Context(), projectID)
	require.NoError(t, err)
	require.Equal(t, "Notion data source "+cliNotionSource, binding.DisplayName)
	sourceTitle.Store([]any{map[string]any{"plain_text": "Tasks"}})
	// Once also uses the long-running daemon client.
	require.Contains(t, runCLI(t, env, dir, "sync", "notion", "once"), "created=1")
	issues, err := env.DB.ListIssues(context.Background(), db.ListIssuesParams{ProjectID: projectID})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	issue := issues[0]
	require.Equal(t, "open", issue.Status)
	require.Equal(t, "[Notion] Imported task", issue.Title)
	binding, err = env.DB.IssueSyncBindingByProject(t.Context(), projectID)
	require.NoError(t, err)
	require.Equal(t, "Tasks", binding.DisplayName)
	require.Contains(t, issue.Body, "Task body")
	editedAt.Store(time.Now().UTC().Format(time.RFC3339Nano))
	completed.Store(true)
	sourceTitle.Store([]any{})
	require.Contains(t, runCLI(t, env, dir, "sync", "notion", "once"), "status_updated=1")
	binding, err = env.DB.IssueSyncBindingByProject(t.Context(), projectID)
	require.NoError(t, err)
	require.Equal(t, "Notion data source "+cliNotionSource, binding.DisplayName)
	closed, err := env.DB.IssueByID(context.Background(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	before := requests.Load()
	runCLI(t, env, dir, "reopen", issue.ShortID)
	reopened, err := env.DB.IssueByID(context.Background(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, "open", reopened.Status)
	runCLI(t, env, dir, "close", issue.ShortID, "--done", "--message", "Completed the imported task and checked its disposable acceptance flow.", "--commit", "deadbeef")
	closed, err = env.DB.IssueByID(context.Background(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	require.Equal(t, before, requests.Load(), "local issue changes must not contact Notion")
	require.Equal(t, int32(2), queries.Load(), "query POST is a read operation")
	require.Zero(t, mutations.Load())
	t.Logf("acceptance: enable -> initial import -> newer completed status -> once -> closed issue -> local reopen/close; %d reads, %d query POSTs, %d upstream mutations", requests.Load(), queries.Load(), mutations.Load())
}

func TestNotionTitlePrefixPresence(t *testing.T) {
	for _, args := range [][]string{nil, {"--title-prefix=false"}, {"--title-prefix=true"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Config map[string]any `json:"config"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				choice, present := body.Config["title_prefix"]
				require.Equal(t, len(args) > 0, present)
				if present {
					require.Equal(t, args[0] == "--title-prefix=true", choice)
				}
				require.NoError(t, json.NewEncoder(w).Encode(cliNotionResponse()))
			})
			runCLI(t, env, dir, append([]string{"sync", "notion", "enable"}, args...)...)
		})
	}
}

func TestNotionTitlePrefixStatus(t *testing.T) {
	for _, mode := range []string{"human", "agent"} {
		for _, choice := range []any{nil, true, false} {
			t.Run(fmt.Sprintf("%s/%v", mode, choice), func(t *testing.T) {
				response := cliNotionResponse()
				config := response["binding"].(map[string]any)["config"].(map[string]any)
				if choice != nil {
					config["title_prefix"] = choice
				}
				env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(response)) })
				args := []string{"sync", "notion", "status"}
				if mode == "agent" {
					args = append([]string{"--agent"}, args...)
				}
				out := runCLI(t, env, dir, args...)
				prefix := "Title prefix: "
				if mode == "agent" {
					prefix = "title_prefix="
				}
				require.Contains(t, out, prefix+fmt.Sprint(choice != false))
			})
		}
	}
}

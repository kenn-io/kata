package twentysync

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// Test the real client against HTTP fixtures; the clock only avoids real pacing.
type testClock struct {
	mu    sync.Mutex
	at    time.Time
	waits []time.Duration
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, delay)
	c.at = c.at.Add(delay)
	return nil
}

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, Config, *testClock) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := testConfig()
	c.APIOrigin = srv.URL
	c.WebOrigin = srv.URL
	clock := &testClock{at: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	client := NewClient(ClientConfig{Daemon: config.TwentySyncConfig{APIOrigin: srv.URL}, LookupEnv: func(name string) (string, bool) {
		require.Equal(t, "KATA_TWENTY_TOKEN", name)
		return "example-token", true
	}, Now: clock.now, Wait: clock.wait})
	return client, c, clock
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	_, err = w.Write(raw)
	require.NoError(t, err)
}

func workspaceResponse(t *testing.T, w http.ResponseWriter, id string) {
	writeJSON(t, w, map[string]any{"data": map[string]any{"currentWorkspace": map[string]any{"id": id, "displayName": "Example workspace"}}})
}

func TestClientRejectsOriginBeforeCredentialLookup(t *testing.T) {
	lookups := 0
	client := NewClient(ClientConfig{Daemon: config.TwentySyncConfig{APIOrigin: "https://twenty.example"}, LookupEnv: func(string) (string, bool) { lookups++; return "secret", true }})
	c := testConfig()
	c.APIOrigin = "https://other.example"
	_, err := client.ForRun(t.Context(), c)
	require.Error(t, err)
	require.Zero(t, lookups)
	require.NotContains(t, err.Error(), "secret")
}

func TestClientWorkspaceAndProvisionalDiscovery(t *testing.T) {
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/metadata", r.URL.Path)
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "Bearer example-token", r.Header.Get("Authorization"))
		workspaceResponse(t, w, workspaceID)
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	workspace, err := session.Workspace(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, workspaceID, workspace.ID)
	c.WorkspaceID = ""
	session, err = client.ForRun(t.Context(), c)
	require.NoError(t, err)
	workspace, err = session.Workspace(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, "Example workspace", workspace.DisplayName)
	_, err = session.Tasks(t.Context(), c)
	require.Error(t, err)
}

func TestClientWorkspaceMismatchStopsTaskRequests(t *testing.T) {
	var tasks atomic.Int32
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			workspaceResponse(t, w, "33333333-3333-4333-8333-333333333333")
			return
		}
		tasks.Add(1)
		w.WriteHeader(500)
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	_, err = session.Tasks(t.Context(), c)
	require.Error(t, err)
	require.Zero(t, tasks.Load())
}

func TestClientNeverFollowsRedirectOrLeaksResponseErrors(t *testing.T) {
	for _, status := range []int{302, 401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var external atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { external.Add(1) }))
			defer target.Close()
			client, c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("remote-secret example-token"))
			})
			session, err := client.ForRun(t.Context(), c)
			require.NoError(t, err)
			_, err = session.Workspace(t.Context(), c)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "remote-secret")
			require.NotContains(t, err.Error(), "example-token")
			require.Zero(t, external.Load())
		})
	}
}

func TestClientPacesRetriesAndHonorsDeadline(t *testing.T) {
	var calls atomic.Int32
	client, c, clock := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(429)
			return
		}
		workspaceResponse(t, w, workspaceID)
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	_, err = session.Workspace(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, int32(3), calls.Load())
	require.Equal(t, []time.Duration{3 * time.Second, 3 * time.Second}, clock.waits)
	_, err = session.Workspace(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, time.Second, clock.waits[len(clock.waits)-1])
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = session.Workspace(ctx, c)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(4), calls.Load())
}

func metadataOperation(t *testing.T, r *http.Request) (string, map[string]any) {
	t.Helper()
	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	require.NoError(t, json.UnmarshalRead(r.Body, &payload))
	return payload.Query, payload.Variables
}

func defaultFields() []map[string]any {
	return []map[string]any{
		{"id": "44444444-4444-4444-8444-444444444441", "name": "title", "type": "TEXT", "isActive": true, "options": nil},
		{"id": "44444444-4444-4444-8444-444444444442", "name": "bodyV2", "type": "RICH_TEXT", "isActive": true, "options": nil},
		{"id": "44444444-4444-4444-8444-444444444443", "name": "status", "type": "SELECT", "isActive": true, "options": []map[string]any{{"value": "TODO"}, {"value": "IN_PROGRESS"}, {"value": "DONE"}}},
	}
}

func connection(nodes []map[string]any, next bool, cursor string) map[string]any {
	edges := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		edges = append(edges, map[string]any{"node": node})
	}
	return map[string]any{"edges": edges, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}}
}

func TestClientMetadataPaginatesObjectsAndFieldsIndependently(t *testing.T) {
	var objectPages, fieldPages atomic.Int32
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		query, vars := metadataOperation(t, r)
		switch {
		case strings.Contains(query, "KataTwentyWorkspace"):
			workspaceResponse(t, w, workspaceID)
		case strings.Contains(query, "KataTwentyObjects"):
			objectPages.Add(1)
			if vars["after"] == nil {
				writeJSON(t, w, map[string]any{"data": map[string]any{"objects": connection([]map[string]any{{"id": "55555555-5555-4555-8555-555555555551", "nameSingular": "person", "isActive": true}}, true, "object-next")}})
				return
			}
			require.Equal(t, "object-next", vars["after"])
			writeJSON(t, w, map[string]any{"data": map[string]any{"objects": connection([]map[string]any{{"id": "55555555-5555-4555-8555-555555555552", "nameSingular": "task", "isActive": true}}, false, "")}})
		case strings.Contains(query, "KataTwentyFields"):
			fieldPages.Add(1)
			require.Equal(t, "55555555-5555-4555-8555-555555555552", vars["id"])
			fields := defaultFields()
			next := vars["after"] == nil
			if next {
				fields = fields[:2]
			} else {
				require.Equal(t, "field-next", vars["after"])
				fields = fields[2:]
			}
			writeJSON(t, w, map[string]any{"data": map[string]any{"object": map[string]any{"id": "55555555-5555-4555-8555-555555555552", "nameSingular": "task", "isActive": true, "fields": connection(fields, next, "field-next")}}})
		default:
			t.Errorf("unexpected metadata query: %s", query)
		}
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	schema, err := session.Schema(t.Context(), c)
	require.NoError(t, err)
	require.Equal(t, []string{"TODO", "IN_PROGRESS", "DONE"}, schema.StatusOptions)
	require.Equal(t, int32(2), objectPages.Load())
	require.Equal(t, int32(2), fieldPages.Load())
}

func taskWire(id string) map[string]any {
	return map[string]any{"id": id, "title": "Example task", "bodyV2": map[string]any{"markdown": "# Task details", "blocknote": nil}, "status": "TODO", "assigneeId": nil, "createdBy": map[string]any{"workspaceMemberId": nil, "source": "API"}, "createdAt": "2026-10-04T00:00:00Z", "updatedAt": "2026-10-04T01:00:00Z", "deletedAt": nil}
}

func TestClientTasksPaginatesAndValidatesEveryPage(t *testing.T) {
	var pages atomic.Int32
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			workspaceResponse(t, w, workspaceID)
			return
		}
		require.Equal(t, "/rest/tasks", r.URL.Path)
		require.Equal(t, "100", r.URL.Query().Get("limit"))
		require.Equal(t, "0", r.URL.Query().Get("depth"))
		pages.Add(1)
		id := taskID
		next := r.URL.Query().Get("starting_after") == ""
		if !next {
			require.Equal(t, "cursor/+opaque", r.URL.Query().Get("starting_after"))
			id = "33333333-3333-4333-8333-333333333333"
		}
		writeJSON(t, w, map[string]any{"data": map[string]any{"tasks": []any{taskWire(id)}}, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": "cursor/+opaque"}})
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	tasks, err := session.Tasks(t.Context(), c)
	require.NoError(t, err)
	require.Len(t, tasks, 2)
	require.Equal(t, "# Task details", tasks[0].Markdown)
	require.Equal(t, int32(2), pages.Load())
}

func TestClientTasksRejectsMalformedContentAndPagination(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(row map[string]any) { delete(row, "status") }, func(row map[string]any) { delete(row, "bodyV2") },
		func(row map[string]any) { row["bodyV2"] = map[string]any{"blocknote": "populated"} },
		func(row map[string]any) { row["bodyV2"] = map[string]any{"markdown": nil, "blocknote": "populated"} },
		func(row map[string]any) { row["status"] = 42 }, func(row map[string]any) { row["createdAt"] = "bad" },
		func(row map[string]any) { row["updatedAt"] = "2025-01-01T00:00:00Z" }, func(row map[string]any) { delete(row, "assigneeId") },
		func(row map[string]any) { row["id"] = "../secret" }, func(row map[string]any) { row["deletedAt"] = "2026-10-04T02:00:00Z" },
	} {
		client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metadata" {
				workspaceResponse(t, w, workspaceID)
				return
			}
			row := taskWire(taskID)
			mutate(row)
			writeJSON(t, w, map[string]any{"data": map[string]any{"tasks": []any{row}}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}})
		})
		session, err := client.ForRun(t.Context(), c)
		require.NoError(t, err)
		tasks, err := session.Tasks(t.Context(), c)
		require.Error(t, err)
		require.Nil(t, tasks)
		require.NotContains(t, err.Error(), "secret")
	}
	for _, body := range []string{`{"data":{"tasks":[]}}`, `{"data":{"tasks":null},"pageInfo":{"hasNextPage":false}}`, `{"data":{"tasks":[]},"pageInfo":{"hasNextPage":true,"endCursor":""}}`} {
		client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metadata" {
				workspaceResponse(t, w, workspaceID)
				return
			}
			_, _ = w.Write([]byte(body))
		})
		session, err := client.ForRun(t.Context(), c)
		require.NoError(t, err)
		_, err = session.Tasks(t.Context(), c)
		require.Error(t, err)
	}
}

// The cutoff excludes old tasks before content checks, so an old task whose
// rich text cannot be imported does not block newer tasks.
func TestClientTasksApplyCutoffBeforeContentChecks(t *testing.T) {
	const newerID = "33333333-3333-4333-8333-333333333333"
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			workspaceResponse(t, w, workspaceID)
			return
		}
		older := taskWire(taskID)
		older["bodyV2"] = map[string]any{"markdown": "", "blocknote": `[{"type":"paragraph","content":[{"type":"text","text":"Old details"}]}]`}
		newer := taskWire(newerID)
		newer["updatedAt"] = "2026-10-06T00:00:00Z"
		writeJSON(t, w, map[string]any{"data": map[string]any{"tasks": []any{older, newer}}, "pageInfo": map[string]any{"hasNextPage": false}})
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	c.Since = "2026-10-05"
	tasks, err := session.Tasks(t.Context(), c)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, newerID, tasks[0].ID)
	c.Since = "2026-10-03"
	_, err = session.Tasks(t.Context(), c)
	require.ErrorContains(t, err, "without Markdown")
}

package todoistsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/issuesync"
)

// apiFixture serves one Todoist project in the shape of the published v1 spec.
type apiFixture struct {
	t               *testing.T
	server          *httptest.Server
	mu              sync.Mutex
	now             time.Time
	row             Task
	account         string
	fail            int
	reads           int
	posts           []string
	paths           []string
	queries         []url.Values
	child           bool
	sectionArchived bool
	lost            bool
	wrong           bool
	reopenOnHistory bool
	nullFields      []string
	// empty answers these paths with HTTP 200 and no body.
	empty           map[string]bool
	projectArchived bool
}

func newAPIFixture(t *testing.T) *apiFixture {
	f := &apiFixture{t: t, now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), row: testTask(), account: testConfig().AccountID}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// taskJSON renders a task as Todoist's ItemSyncView, with null for absent values.
func taskJSON(t Task, nullFields ...string) map[string]any {
	orNull := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	v := map[string]any{
		"id": t.ID, "project_id": t.ProjectID, "parent_id": orNull(t.ParentID), "section_id": orNull(t.SectionID),
		"content": t.Content, "description": t.Description, "added_by_uid": orNull(t.AddedBy), "responsible_uid": orNull(t.Assignee),
		"labels": append([]string{}, t.Labels...), "priority": t.Priority, "checked": t.Checked, "is_deleted": t.Deleted,
		"added_at": t.AddedAt.Format(time.RFC3339Nano), "updated_at": t.UpdatedAt.Format(time.RFC3339Nano), "completed_at": nil, "due": nil,
	}
	if t.CompletedAt != nil {
		v["completed_at"] = t.CompletedAt.Format(time.RFC3339Nano)
	}
	if t.Recurring {
		v["due"] = map[string]any{"date": "2026-10-08", "is_recurring": true}
	}
	for _, key := range nullFields {
		v[key] = nil
	}
	return v
}

func (f *apiFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer fixture-secret" {
		f.t.Error("missing selected credential")
	}
	f.reads++
	f.paths = append(f.paths, r.URL.Path)
	if f.fail > 0 {
		f.fail--
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if f.empty[r.URL.Path] {
		w.WriteHeader(http.StatusOK)
		return
	}
	reply := func(v any) { raw, err := json.Marshal(v); require.NoError(f.t, err); _, _ = w.Write(raw) }
	inProject := f.row.ProjectID == testConfig().ProjectID
	switch r.URL.Path {
	case "/api/v1/user":
		reply(map[string]any{"id": f.account, "email": "person@example.com"})
	case "/api/v1/projects/" + testConfig().ProjectID:
		reply(map[string]any{"id": testConfig().ProjectID, "name": "Example tasks", "is_archived": f.projectArchived, "is_deleted": false})
	case "/api/v1/tasks":
		f.queries = append(f.queries, r.URL.Query())
		rows := []any{}
		parent := r.URL.Query().Get("parent_id")
		if !f.row.Checked && inProject && parent == "" {
			rows = append(rows, taskJSON(f.row, f.nullFields...))
		}
		if f.child && (parent == "" || parent == f.row.ID) {
			child := testTask()
			child.ID, child.ParentID = "child123", f.row.ID
			rows = append(rows, taskJSON(child))
		}
		reply(map[string]any{"results": rows, "next_cursor": nil})
	case "/api/v1/tasks/completed/by_completion_date":
		f.queries = append(f.queries, r.URL.Query())
		since, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
		until, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("until"))
		items := []any{}
		if f.row.Checked && inProject && !f.row.CompletedAt.Before(since) && f.row.CompletedAt.Before(until) {
			items = append(items, taskJSON(f.row, f.nullFields...))
		}
		// The spec omits next_cursor on the last page.
		reply(map[string]any{"items": items})
		if f.reopenOnHistory && len(items) > 0 {
			f.row.Checked, f.row.CompletedAt = false, nil
		}
	case "/api/v1/tasks/" + testTask().ID:
		// Todoist serves an active task here even after it moves to another project.
		if f.row.Checked {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(taskJSON(f.row, f.nullFields...))
	case "/api/v1/sections/section123":
		reply(map[string]any{"id": "section123", "project_id": testConfig().ProjectID, "is_archived": f.sectionArchived, "is_deleted": false})
	case "/api/v1/tasks/" + testTask().ID + "/close", "/api/v1/tasks/" + testTask().ID + "/reopen":
		require.Equal(f.t, http.MethodPost, r.Method)
		f.posts = append(f.posts, r.URL.Path)
		f.now = f.now.Add(time.Second)
		f.row.UpdatedAt = f.now
		f.row.Checked = r.URL.Path[len(r.URL.Path)-5:] == "close"
		f.row.CompletedAt = nil
		if f.row.Checked {
			f.row.CompletedAt = new(f.now)
		}
		if f.wrong {
			f.row.ProjectID = "foreign"
		}
		if f.lost {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("{}"))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *apiFixture) client() *Client {
	return NewClient(ClientConfig{
		Daemon: config.TodoistSyncConfig{APIOrigin: f.server.URL, TokenEnv: "EXAMPLE_TOKEN"},
		LookupEnv: func(key string) (string, bool) {
			require.Equal(f.t, "EXAMPLE_TOKEN", key)
			return "fixture-secret", true
		},
		BackOff: func() backoff.BackOff { return &backoff.ZeroBackOff{} },
	})
}

func (f *apiFixture) cfg() Config { c := testConfig(); c.APIOrigin = f.server.URL; return c }

func (f *apiFixture) completeInTodoist() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(time.Minute)
	f.row.Checked = true
	f.row.CompletedAt = new(f.now)
	f.row.UpdatedAt = f.now
	f.now = f.now.Add(time.Minute)
}

func (f *apiFixture) requestsTo(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if p == path {
			n++
		}
	}
	return n
}

// Contract: Tasks follows both pagination styles, keeps every history request
// within Todoist's three-month range, and lets active state win over history.
func TestClientTasksPaginatesAndMergesHistory(t *testing.T) {
	at := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	item := func(id string, completed bool) map[string]any {
		task := testTask()
		task.ID = id
		if completed {
			task.Checked, task.CompletedAt = true, new(at)
		}
		return taskJSON(task)
	}
	var windows [][2]time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		var body any
		switch r.URL.Path {
		case "/api/v1/user":
			body = map[string]any{"id": testConfig().AccountID}
		case "/api/v1/tasks":
			body = map[string]any{"results": []any{item("active-a", false)}, "next_cursor": "a.b"}
			if q.Get("cursor") == "a.b" {
				body = map[string]any{"results": []any{item("active-b", false)}, "next_cursor": nil}
			}
		case "/api/v1/tasks/completed/by_completion_date":
			since, _ := time.Parse(time.RFC3339Nano, q.Get("since"))
			until, _ := time.Parse(time.RFC3339Nano, q.Get("until"))
			windows = append(windows, [2]time.Time{since, until})
			body = map[string]any{"items": []any{}}
			if at.Before(since) || !at.Before(until) {
				break
			}
			if q.Get("cursor") == "" {
				body = map[string]any{"items": []any{item("active-a", true)}, "next_cursor": "c.d"}
			} else {
				body = map[string]any{"items": []any{item("closed-c", true)}}
			}
		}
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	c := testConfig()
	c.APIOrigin = srv.URL
	c.HistorySince = "2025-11-01"
	client := NewClient(ClientConfig{Daemon: config.TodoistSyncConfig{APIOrigin: srv.URL}, LookupEnv: func(string) (string, bool) { return "fixture-secret", true }})
	s, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	rows, err := s.Tasks(t.Context(), c, time.Time{}, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = row.Checked
	}
	require.Equal(t, map[string]bool{"active-a": false, "active-b": false, "closed-c": true}, got)
	require.Equal(t, time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC), windows[0][0])
	for _, w := range windows {
		require.False(t, w[1].After(w[0].AddDate(0, 3, 0)))
	}
}

// Contract: Todoist documents added_at and updated_at as null when unknown;
// those tasks still import with times taken from their other timestamps.
func TestClientImportsTasksWithUnknownTimestamps(t *testing.T) {
	for _, fields := range [][]string{{"updated_at"}, {"added_at"}, {"added_at", "updated_at"}} {
		f := newAPIFixture(t)
		f.nullFields = fields
		c := f.cfg()
		s, err := f.client().ForRun(t.Context(), c)
		require.NoError(t, err)
		rows, err := s.Tasks(t.Context(), c, time.Time{}, f.now)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		_, err = BuildImportBatch(c, rows)
		require.NoError(t, err, "null %v", fields)
	}
}

// Contract: rate-limited reads retry, and an exhausted retry reports the rate
// limit so the status runner can defer instead of failing the binding.
func TestClientRetriesRateLimitedReads(t *testing.T) {
	f := newAPIFixture(t)
	f.fail = 2
	_, err := f.client().ForRun(t.Context(), f.cfg())
	require.NoError(t, err)
	require.Equal(t, 3, f.reads)

	f = newAPIFixture(t)
	f.fail = 10
	_, err = f.client().ForRun(t.Context(), f.cfg())
	statusErr, ok := errors.AsType[*issuesync.StatusError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, http.StatusTooManyRequests, statusErr.HTTPStatus)
	require.False(t, statusErr.Blocked)
	require.Equal(t, 4, f.reads)
}

func TestClientRejectsCredentialedRedirect(t *testing.T) {
	var leaked bool
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { leaked = true }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := NewClient(ClientConfig{Daemon: config.TodoistSyncConfig{APIOrigin: source.URL}, LookupEnv: func(string) (string, bool) { return "fixture-secret", true }})
	_, err := client.Account(t.Context())
	require.Error(t, err)
	require.False(t, leaked)
}

// Contract: authentication and permission failures block after one request.
func TestClientAuthenticationFailureIsTerminal(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(code)
		}))
		c := testConfig()
		c.APIOrigin = server.URL
		client := NewClient(ClientConfig{Daemon: config.TodoistSyncConfig{APIOrigin: server.URL}, LookupEnv: func(string) (string, bool) { return "fixture-secret", true }})
		_, err := client.ForRun(context.Background(), c)
		statusErr, ok := errors.AsType[*issuesync.StatusError](err)
		require.True(t, ok, "%v", err)
		require.True(t, statusErr.Blocked)
		require.Contains(t, err.Error(), fmt.Sprintf("HTTP %d", code))
		require.Equal(t, 1, requests)
		server.Close()
	}
}

// Contract: Todoist's task view carries is_deleted; deleted tasks in
// completion history never import.
func TestClientSkipsDeletedCompletions(t *testing.T) {
	f := newAPIFixture(t)
	f.row.Checked, f.row.CompletedAt, f.row.Deleted = true, new(f.row.UpdatedAt), true
	c := f.cfg()
	s, err := f.client().ForRun(t.Context(), c)
	require.NoError(t, err)
	rows, err := s.Tasks(t.Context(), c, time.Time{}, f.now)
	require.NoError(t, err)
	require.Empty(t, rows)
}

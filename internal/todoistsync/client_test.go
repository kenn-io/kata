package todoistsync

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

type apiFixture struct {
	t                  *testing.T
	server             *httptest.Server
	mu                 sync.Mutex
	now                time.Time
	row                Task
	account            string
	archived           bool
	fail               int
	reads              int
	posts              []string
	queries            []url.Values
	active             bool
	recurring          bool
	child              bool
	sectionArchived    bool
	lost               bool
	wrong              bool
	reopenHistory      bool
	omitDue            bool
	omitRecurring      bool
	omitHierarchy      bool
	omitChildHierarchy bool
	paths              []string
	nullFields         []string
}

func newAPIFixture(t *testing.T) *apiFixture {
	f := &apiFixture{t: t, now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), row: testTask(), account: testConfig().AccountID, active: true}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
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
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = fmt.Fprint(w, "private-body-secret")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) { raw, err := json.Marshal(v); require.NoError(f.t, err); _, _ = w.Write(raw) }
	switch r.URL.Path {
	case "/api/v1/user":
		reply(map[string]any{"id": f.account})
	case "/api/v1/projects/" + testConfig().ProjectID:
		reply(map[string]any{"id": testConfig().ProjectID, "name": "Example tasks", "is_archived": f.archived, "is_deleted": false})
	case "/api/v1/tasks":
		f.queries = append(f.queries, r.URL.Query())
		rows := []Task{}
		if f.active {
			row := f.row
			if f.recurring {
				row.Due = &Due{Recurring: true}
			}
			rows = append(rows, row)
		}
		if f.child {
			child := testTask()
			child.ID = "child123"
			child.ParentID = f.row.ID
			rows = append(rows, child)
		}
		if parent := r.URL.Query().Get("parent_id"); parent != "" {
			rows = slices.DeleteFunc(rows, func(row Task) bool { return row.ParentID != parent })
		}
		if len(f.nullFields) > 0 {
			raw, err := json.Marshal(rows)
			require.NoError(f.t, err)
			var fields []map[string]any
			require.NoError(f.t, json.Unmarshal(raw, &fields))
			for _, row := range fields {
				for _, key := range f.nullFields {
					row[key] = nil
				}
			}
			reply(map[string]any{"results": fields, "next_cursor": nil})
		} else if f.omitChildHierarchy {
			raw, err := json.Marshal(rows)
			require.NoError(f.t, err)
			var fields []map[string]any
			require.NoError(f.t, json.Unmarshal(raw, &fields))
			for _, row := range fields {
				if row["id"] == "child123" {
					delete(row, "parent_id")
				}
			}
			reply(map[string]any{"results": fields, "next_cursor": nil})
		} else {
			reply(map[string]any{"results": rows, "next_cursor": nil})
		}
	case "/api/v1/tasks/completed/by_completion_date":
		f.queries = append(f.queries, r.URL.Query())
		rows := []Task{}
		until, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("until"))
		since, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
		if !f.active && f.row.CompletedAt != nil && !f.row.CompletedAt.Before(since) && f.row.CompletedAt.Before(until) {
			rows = append(rows, f.row)
		}
		// Todoist omits next_cursor on the last completed-history page.
		if len(f.nullFields) == 0 {
			reply(map[string]any{"items": rows})
		} else {
			raw, err := json.Marshal(rows)
			require.NoError(f.t, err)
			var fields []map[string]any
			require.NoError(f.t, json.Unmarshal(raw, &fields))
			for _, row := range fields {
				for _, key := range f.nullFields {
					row[key] = nil
				}
			}
			reply(map[string]any{"items": fields})
		}
		if f.reopenHistory && len(rows) > 0 {
			f.active = true
			f.row.Checked = new(false)
			f.row.CompletedAt = nil
			f.row.UpdatedAt = f.now
		}

	case "/api/v1/tasks/" + testTask().ID:
		if !f.active {
			w.WriteHeader(404)
			_, _ = fmt.Fprint(w, `{"error":"private-body-secret"}`)
			return
		}
		row := f.row
		if f.recurring {
			row.Due = &Due{Recurring: true}
		}
		if f.omitDue || f.omitRecurring || f.omitHierarchy {
			raw, err := json.Marshal(row)
			require.NoError(f.t, err)
			var fields map[string]any
			require.NoError(f.t, json.Unmarshal(raw, &fields))
			if f.omitDue {
				delete(fields, "due")
			} else if f.omitRecurring {
				fields["due"] = map[string]any{"date": "2026-10-08"}
			}
			if f.omitHierarchy {
				delete(fields, "parent_id")
				delete(fields, "section_id")
			}
			reply(fields)
		} else if len(f.nullFields) > 0 {
			raw, err := json.Marshal(row)
			require.NoError(f.t, err)
			var fields map[string]any
			require.NoError(f.t, json.Unmarshal(raw, &fields))
			for _, key := range f.nullFields {
				fields[key] = nil
			}
			reply(fields)
		} else {
			reply(row)
		}
	case "/api/v1/sections/section123":
		reply(map[string]any{"id": "section123", "project_id": testConfig().ProjectID, "is_archived": f.sectionArchived, "is_deleted": false})
	case "/api/v1/tasks/" + testTask().ID + "/close", "/api/v1/tasks/" + testTask().ID + "/reopen":
		require.Equal(f.t, http.MethodPost, r.Method)
		f.posts = append(f.posts, r.URL.Path)
		f.now = f.now.Add(time.Second)
		f.row.UpdatedAt = f.now
		f.active = r.URL.Path[len(r.URL.Path)-6:] == "reopen"
		f.row.Checked = new(!f.active)
		if f.active {
			f.row.CompletedAt = nil
		} else {
			f.row.CompletedAt = new(f.now)
		}
		// The response reaches the client after Todoist stamps the change.
		f.now = f.now.Add(time.Millisecond)
		if f.lost {
			w.WriteHeader(503)
			_, _ = fmt.Fprint(w, "private-body-secret")
			return
		}
		if f.wrong {
			f.row.ProjectID = "foreign"
		}
		_, _ = fmt.Fprint(w, "null")
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *apiFixture) client() *Client {
	return NewClient(ClientConfig{Daemon: config.TodoistSyncConfig{APIOrigin: f.server.URL, TokenEnv: "EXAMPLE_TOKEN"}, LookupEnv: func(key string) (string, bool) {
		require.Equal(f.t, "EXAMPLE_TOKEN", key)
		return "fixture-secret", true
	}, Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }, Wait: func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.now = f.now.Add(d)
		f.mu.Unlock()
		return ctx.Err()
	}})
}
func (f *apiFixture) cfg() Config { c := testConfig(); c.APIOrigin = f.server.URL; return c }
func TestClientScopedTasksAndHistoryWindows(t *testing.T) {
	f := newAPIFixture(t)
	c := f.cfg()
	client := f.client()
	s, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)
	rows, err := s.Tasks(context.Background(), c, time.Time{}, f.now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, testTask().ID, rows[0].ID)
	require.Len(t, f.queries, 3)
	for _, q := range f.queries {
		require.Equal(t, c.ProjectID, q.Get("project_id"))
		require.Equal(t, "200", q.Get("limit"))
		if q.Has("since") {
			since, _ := time.Parse(time.RFC3339Nano, q.Get("since"))
			until, _ := time.Parse(time.RFC3339Nano, q.Get("until"))
			require.LessOrEqual(t, until.Sub(since), 30*24*time.Hour)
		}
	}
}

func TestClientTasksPrefersReopenedTaskAfterHistoryRead(t *testing.T) {
	f := newAPIFixture(t)
	f.active = false
	f.row.Checked = new(true)
	f.row.CompletedAt = new(f.now.Add(-time.Hour))
	f.row.UpdatedAt = *f.row.CompletedAt
	f.reopenHistory = true
	c := f.cfg()
	session, err := f.client().ForRun(context.Background(), c)
	require.NoError(t, err)

	rows, err := session.Tasks(context.Background(), c, time.Time{}, f.now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.False(t, *rows[0].Checked, "the active observation after history pagination must win over its stale completion")
	require.Nil(t, rows[0].CompletedAt)

	activeReads := 0
	for _, query := range f.queries {
		if !query.Has("since") {
			activeReads++
		}
	}
	require.Equal(t, 1, activeReads, "active tasks must be read after the completion-history scan")
}

func TestClientAccountOriginAndAuthGuards(t *testing.T) {
	f := newAPIFixture(t)
	client := f.client()
	c := f.cfg()
	c.APIOrigin = "https://api.todoist.com"
	_, err := client.ForRun(context.Background(), c)
	require.Error(t, err)
	require.Zero(t, f.reads)
	c = f.cfg()
	f.account = "different"
	_, err = client.ForRun(context.Background(), c)
	require.Error(t, err)
	require.Empty(t, f.posts)
	f.account = c.AccountID
	client = NewClient(ClientConfig{Daemon: config.TodoistSyncConfig{APIOrigin: f.server.URL, TokenEnv: "EXAMPLE_TOKEN"}, LookupEnv: func(string) (string, bool) { return "", false }})
	_, err = client.ForRun(context.Background(), c)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "fixture-secret")
}
func TestClientReadRetryAndDeletedProject(t *testing.T) {
	f := newAPIFixture(t)
	f.fail = 1
	s, err := f.client().ForRun(context.Background(), f.cfg())
	require.NoError(t, err)
	require.Equal(t, 2, f.reads)
	f.archived = true
	_, err = s.Project(context.Background(), f.cfg())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-body-secret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Tasks(ctx, f.cfg(), time.Time{}, f.now)
	require.ErrorIs(t, err, context.Canceled)
}
func TestMergeTasksActiveWinsAndDeterministicHistory(t *testing.T) {
	active := testTask()
	closed := testTask()
	closed.Checked = new(true)
	closed.CompletedAt = new(closed.UpdatedAt)
	rows, err := mergeTasks(testConfig(), []Task{active}, []Task{closed})
	require.NoError(t, err)
	require.False(t, *rows[0].Checked)
	newer := closed
	newer.UpdatedAt = newer.UpdatedAt.Add(time.Hour)
	for _, history := range [][]Task{{closed, newer}, {newer, closed}} {
		rows, err := mergeTasks(testConfig(), nil, history)
		require.NoError(t, err)
		require.Equal(t, newer.UpdatedAt, rows[0].UpdatedAt)
	}
	other := closed
	other.Content = "conflicting"
	_, err = mergeTasks(testConfig(), nil, []Task{closed, other})
	require.Error(t, err)
}

func TestClientCursorIsDataAndRejectsRepeats(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "/api/v1/tasks", r.URL.Path)
		require.Equal(t, testConfig().ProjectID, r.URL.Query().Get("project_id"))
		w.Header().Set("Content-Type", "application/json")
		if requests > 1 {
			require.Equal(t, "https://foreign.example/?secret", r.URL.Query().Get("cursor"))
		}
		_, _ = fmt.Fprint(w, `{"results":[],"next_cursor":"https://foreign.example/?secret"}`)
	}))
	defer srv.Close()
	f := newAPIFixture(t)
	client := f.client()
	s := &clientSession{client: client, config: f.cfg(), token: "fixture-secret"}
	s.config.APIOrigin = srv.URL
	c := s.config
	_, err := s.taskPages(context.Background(), c, "/api/v1/tasks", "results", url.Values{}, &readBudget{})
	require.ErrorContains(t, err, "repeats")
	require.Equal(t, 2, requests)
	require.NotContains(t, err.Error(), "secret")
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
	_, err := client.Account(context.Background())
	require.Error(t, err)
	require.False(t, leaked)
}

// Contract: provider authentication/permission failures stop after one request and redact bodies.
func TestClientAuthenticationFailureIsTerminal(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			require.Equal(t, "/api/v1/user", r.URL.Path)
			w.WriteHeader(code)
			_, _ = fmt.Fprint(w, "private-body-secret")
		}))
		c := testConfig()
		c.APIOrigin = server.URL
		client := NewClient(ClientConfig{Daemon: config.TodoistSyncConfig{APIOrigin: server.URL}, LookupEnv: func(string) (string, bool) { return "fixture-secret", true }})
		_, err := client.ForRun(context.Background(), c)
		require.Error(t, err)
		require.Contains(t, err.Error(), fmt.Sprintf("HTTP %d", code))
		require.NotContains(t, err.Error(), "secret")
		require.Equal(t, 1, requests)
		server.Close()
	}
}

// Contract: exhausted 429 reads stop after the initial attempt and three bounded retries.
func TestClientRateLimitRetryBudget(t *testing.T) {
	f := newAPIFixture(t)
	f.fail = 10
	_, err := f.client().ForRun(context.Background(), f.cfg())
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 429")
	require.NotContains(t, err.Error(), "secret")
	require.Equal(t, 4, f.reads)
}

// Contract: Todoist documents added_at and updated_at as null when unknown.
func TestClientImportsTasksWithUnknownTimestamps(t *testing.T) {
	for _, fields := range [][]string{{"updated_at"}, {"added_at"}} {
		f := newAPIFixture(t)
		f.nullFields = fields
		c := f.cfg()
		s, err := f.client().ForRun(context.Background(), c)
		require.NoError(t, err)
		rows, err := s.Tasks(context.Background(), c, time.Time{}, f.now)
		require.NoError(t, err, "null %v", fields)
		require.Len(t, rows, 1)
		require.Equal(t, testTask().AddedAt, rows[0].AddedAt)
		require.Equal(t, testTask().UpdatedAt, rows[0].UpdatedAt)
		batch, err := BuildImportBatch(c.SourceKey(), c, Project{ID: c.ProjectID}, rows)
		require.NoError(t, err)
		if slices.Contains(fields, "updated_at") {
			require.True(t, batch.ReconcileUnknownSourceTimestamp["task:"+rows[0].ID])
		} else {
			require.False(t, batch.ReconcileUnknownSourceTimestamp["task:"+rows[0].ID])
		}
	}
	f := newAPIFixture(t)
	f.nullFields = []string{"added_at", "updated_at"}
	c := f.cfg()
	s, err := f.client().ForRun(context.Background(), c)
	require.NoError(t, err)
	_, err = s.Tasks(context.Background(), c, time.Time{}, f.now)
	require.Error(t, err)
}

package todoistsync

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestProjectAPIPathPreservesTodoistEndpoint(t *testing.T) {
	require.Equal(t, "/api/v1/projects/project123", projectAPIPath("project123"))
}

func TestStatusReadsReuseCompletedHistoryAcrossMappings(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	floor := base.Add(-365 * 24 * time.Hour)
	f := newStatusAPIHarness(t, base)
	for i, id := range []string{"task123", "task456", "task789"} {
		completedAt := base.Add(-time.Duration(i+1) * 24 * time.Hour)
		f.tasks[id] = statusTask(id, completedAt, true)
		f.active[id] = false
	}
	c, session := f.session(t, floor)
	status, ok := session.(StatusSession)
	require.True(t, ok)

	for _, id := range []string{"task123", "task456", "task789"} {
		observation, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: id})
		require.NoError(t, err)
		require.Equal(t, "closed", observation.Status)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.historyRequests, "recent completions for all mappings should share one bounded history window")
	for _, id := range []string{"task123", "task456", "task789"} {
		require.Equal(t, 2, f.activeReads[id], "each mapping keeps its active-task rereads around history lookup")
	}
}

func TestStatusWriteRefreshesCachedHistoryAfterDispatch(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := newStatusAPIHarness(t, base)
	f.tasks["task123"] = statusTask("task123", base.Add(-time.Hour), true)
	f.active["task123"] = false
	f.tasks["task456"] = statusTask("task456", time.Time{}, false)
	f.active["task456"] = true
	c, session := f.session(t, base.Add(-7*24*time.Hour))
	c.StatusSync = "two-way"
	status, ok := session.(StatusSession)
	require.True(t, ok)

	_, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: "task123"})
	require.NoError(t, err)
	firstHistoryRequests := f.historyRequestCount()
	require.Equal(t, 1, firstHistoryRequests)

	observation, err := status.WriteStatus(context.Background(), c, StatusTarget{ID: "task456"}, "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", observation.Status)
	require.Equal(t, firstHistoryRequests+1, f.historyRequestCount(), "verification must refresh history after the dispatched close")
}

func TestStatusReopenRefreshesCompletionHistoryBeforeSafety(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Task)
	}{
		{name: "moved-outside-selected-project", change: func(row *Task) { row.ProjectID = "project456" }},
		{name: "reparented-after-cache-population", change: func(row *Task) { row.ParentID = "parent456" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			f := newStatusAPIHarness(t, base)
			f.tasks["task123"] = statusTask("task123", base.Add(-time.Hour), true)
			f.active["task123"] = false
			c, session := f.session(t, base.Add(-7*24*time.Hour))
			c.StatusSync = "two-way"
			status, ok := session.(StatusSession)
			require.True(t, ok)

			observed, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: "task123"})
			require.NoError(t, err)
			require.Equal(t, "closed", observed.Status)
			firstHistoryRequests := f.historyRequestCount()
			require.Equal(t, 1, firstHistoryRequests)

			f.mu.Lock()
			changed := f.tasks["task123"]
			tc.change(&changed)
			f.tasks["task123"] = changed
			f.mu.Unlock()

			_, err = status.WriteStatus(context.Background(), c, StatusTarget{ID: "task123"}, "open", func() error { return nil })
			require.Error(t, err)
			require.Equal(t, 0, f.statusWriteCount(), "stale scope or hierarchy evidence must never authorize a reopen POST")
			require.Equal(t, firstHistoryRequests+1, f.historyRequestCount(), "reopen safety must use a fresh completion-history response")
		})
	}
}

type statusAPIHarness struct {
	t               *testing.T
	server          *httptest.Server
	mu              sync.Mutex
	now             time.Time
	tasks           map[string]Task
	active          map[string]bool
	activeReads     map[string]int
	historyRequests int
	statusWrites    int
}

func newStatusAPIHarness(t *testing.T, now time.Time) *statusAPIHarness {
	t.Helper()
	f := &statusAPIHarness{t: t, now: now, tasks: map[string]Task{}, active: map[string]bool{}, activeReads: map[string]int{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *statusAPIHarness) session(t *testing.T, historySince time.Time) (Config, Session) {
	t.Helper()
	c := testConfig()
	c.APIOrigin = f.server.URL
	c.HistorySince = historySince.Format(time.RFC3339)
	client := NewClient(ClientConfig{
		Daemon:    config.TodoistSyncConfig{APIOrigin: f.server.URL, TokenEnv: "EXAMPLE_TOKEN"},
		LookupEnv: func(string) (string, bool) { return "fixture-secret", true },
		Now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
		Wait: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.mu.Lock()
			f.now = f.now.Add(d)
			f.mu.Unlock()
			return nil
		},
	})
	session, err := client.ForRun(context.Background(), c)
	require.NoError(t, err)
	return c, session
}

func statusTask(id string, completedAt time.Time, checked bool) Task {
	row := testTask()
	row.ID = id
	row.Checked = new(checked)
	if !completedAt.IsZero() {
		row.CompletedAt = new(completedAt)
		row.UpdatedAt = completedAt
	}
	return row
}

func (f *statusAPIHarness) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer fixture-secret" {
		f.t.Error("missing selected credential")
	}
	writeJSON := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		raw, err := json.Marshal(value)
		if err != nil {
			f.t.Errorf("marshal fixture response: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, err = w.Write(raw)
		if err != nil {
			f.t.Errorf("write fixture response: %v", err)
		}
	}
	switch {
	case r.URL.Path == "/api/v1/user":
		writeJSON(map[string]any{"id": testConfig().AccountID})
	case r.URL.Path == projectAPIPath("project123"):
		writeJSON(map[string]any{"id": "project123", "name": "Example tasks", "is_archived": false, "is_deleted": false})
	case r.URL.Path == "/api/v1/tasks/completed/by_completion_date":
		since, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
		if err != nil {
			f.t.Errorf("parse history since: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		until, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("until"))
		if err != nil {
			f.t.Errorf("parse history until: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.historyRequests++
		rows := make([]Task, 0, len(f.tasks))
		projectID := r.URL.Query().Get("project_id")
		for id, row := range f.tasks {
			if !f.active[id] && row.ProjectID == projectID && row.CompletedAt != nil && !row.CompletedAt.Before(since) && row.CompletedAt.Before(until) {
				rows = append(rows, row)
			}
		}
		f.mu.Unlock()
		writeJSON(map[string]any{"items": rows, "next_cursor": nil})
	case r.URL.Path == "/api/v1/tasks":
		f.mu.Lock()
		rows := make([]Task, 0, len(f.tasks))
		projectID := r.URL.Query().Get("project_id")
		for id, row := range f.tasks {
			if f.active[id] && row.ProjectID == projectID {
				rows = append(rows, row)
			}
		}
		f.mu.Unlock()
		writeJSON(map[string]any{"results": rows, "next_cursor": nil})
	case strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
		idOrAction := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		if r.Method == http.MethodPost && strings.HasSuffix(idOrAction, "/reopen") {
			id := strings.TrimSuffix(idOrAction, "/reopen")
			f.mu.Lock()
			row, ok := f.tasks[id]
			if !ok {
				f.mu.Unlock()
				f.t.Errorf("reopen requested for unknown task %s", id)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.statusWrites++
			row.Checked = new(false)
			f.tasks[id] = row
			f.active[id] = true
			f.mu.Unlock()
			_, _ = fmt.Fprint(w, "null")
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(idOrAction, "/close") {
			id := strings.TrimSuffix(idOrAction, "/close")
			f.mu.Lock()
			row, ok := f.tasks[id]
			if !ok {
				f.mu.Unlock()
				f.t.Errorf("close requested for unknown task %s", id)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			completedAt := f.now.Add(-time.Second)
			row.Checked = new(true)
			row.CompletedAt = new(completedAt)
			row.UpdatedAt = completedAt
			f.tasks[id] = row
			f.active[id] = false
			f.mu.Unlock()
			_, _ = fmt.Fprint(w, "null")
			return
		}
		f.mu.Lock()
		f.activeReads[idOrAction]++
		row, ok := f.tasks[idOrAction]
		active := f.active[idOrAction]
		f.mu.Unlock()
		if !ok || !active {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(row)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *statusAPIHarness) historyRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.historyRequests
}

func (f *statusAPIHarness) statusWriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusWrites
}

// Contract: a task completed after the run's cached history is found by
// reading only the uncovered tail, not by replaying cached history.
func TestStatusHistoryMissReadsOnlyNewTail(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := newStatusAPIHarness(t, base)
	f.tasks["task123"] = statusTask("task123", base.Add(-time.Hour), true)
	f.tasks["task456"] = statusTask("task456", time.Time{}, false)
	f.active["task456"] = true
	c, session := f.session(t, base.Add(-60*24*time.Hour))
	status, ok := session.(StatusSession)
	require.True(t, ok)
	open := "open"
	prior := &db.IssueStatusObservation{Raw: &open, Version: base.Add(-2 * time.Hour)}

	observed, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: "task123", Prior: prior})
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
	require.Equal(t, 1, f.historyRequestCount())

	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	f.tasks["task456"] = statusTask("task456", f.now.Add(-time.Second), true)
	f.active["task456"] = false
	f.mu.Unlock()
	observed, err = status.ReadStatus(context.Background(), c, StatusTarget{ID: "task456", Prior: prior})
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
	require.Equal(t, 2, f.historyRequestCount(), "one request for the uncovered tail")
}

// Contract: a narrow history read after a nearly full cache is bounded by the
// new observations, not by the size of the session cache.
func TestStatusHistoryTailDoesNotApplyContentLimitToCache(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	floor := base.Add(-365 * 24 * time.Hour)
	f := newStatusAPIHarness(t, base)
	for i := range maxItems - 1 {
		id := fmt.Sprintf("fill%05d", i)
		f.tasks[id] = statusTask(id, base.Add(-time.Hour), true)
		f.active[id] = false
	}
	c, session := f.session(t, floor)
	status, ok := session.(StatusSession)
	require.True(t, ok)

	first, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: "fill00000"})
	require.NoError(t, err)
	require.Equal(t, "closed", first.Status)
	require.Equal(t, 1, f.historyRequestCount())

	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	for i, id := range []string{"new123", "new456"} {
		f.tasks[id] = statusTask(id, base.Add(time.Duration(i+1)*20*time.Second), true)
		f.active[id] = false
	}
	f.mu.Unlock()
	open := "open"
	prior := &db.IssueStatusObservation{Raw: &open, Version: base.Add(-5 * time.Minute)}
	second, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: "new123", Prior: prior})
	require.NoError(t, err)
	require.Equal(t, "closed", second.Status)
	require.Equal(t, 2, f.historyRequestCount(), "only the uncovered tail should be read")

	third, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: "fill00001"})
	require.NoError(t, err)
	require.Equal(t, "closed", third.Status)
	require.Equal(t, 2, f.historyRequestCount(), "the cache remains usable after exceeding the content batch limit")
}

// Contract: an old open observation cannot make a recent completion
// unreachable when the older history contains more rows than one read budget.
func TestStatusHistoryFindsRecentCompletionBeforeLargeOlderHistory(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	floor := base.Add(-365 * 24 * time.Hour)
	f := newStatusAPIHarness(t, base)
	f.tasks["task123"] = statusTask("task123", base.Add(-24*time.Hour), true)
	f.active["task123"] = false
	for i := range maxItems + 1 {
		id := fmt.Sprintf("fill%05d", i)
		f.tasks[id] = statusTask(id, base.Add(-60*24*time.Hour), true)
		f.active[id] = false
	}
	c, session := f.session(t, floor)
	status, ok := session.(StatusSession)
	require.True(t, ok)
	open := "open"
	prior := &db.IssueStatusObservation{Raw: &open, Version: floor}

	observed, err := status.ReadStatus(context.Background(), c, StatusTarget{ID: "task123", Prior: prior})
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
	require.Equal(t, 1, f.historyRequestCount(), "the exact recent completion should stop the backwards history scan")
}

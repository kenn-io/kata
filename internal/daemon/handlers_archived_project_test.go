package daemon_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchivedProject_SurfaceHandlersReturn404 covers every project-scoped
// surface handler that calls activeProjectByID. Each must hide an archived
// project behind a project_not_found 404, matching the behavior already
// pinned for createIssue.
//
// Internal callers (merge, restore, alias resolve) deliberately use
// store.ProjectByID directly to operate on archived rows; those paths are
// not exercised here.
func TestArchivedProject_SurfaceHandlersReturn404(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)

	// Archive the project directly via the DB layer. The HTTP DELETE path is
	// covered by handlers_projects_test; here we only need the projects row
	// in archived state so we can probe each surface endpoint.
	archiveProject(t, h, projectID, false)

	pid := strconv.FormatInt(projectID, 10)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"showProject", http.MethodGet, "/api/v1/projects/" + pid, nil},
		{"renameProject", http.MethodPatch, "/api/v1/projects/" + pid,
			map[string]any{"name": "renamed", "actor": "user-a"}},
		// resetIssueCounter case removed: endpoint deleted in spec §9.5.
		{"createIssue", http.MethodPost, "/api/v1/projects/" + pid + "/issues",
			map[string]any{"actor": "tester", "title": "x", "body": ""}},
		{"importIssues", http.MethodPost, "/api/v1/projects/" + pid + "/imports",
			map[string]any{
				"actor":  "importer",
				"source": "beads",
				"items": []map[string]any{{
					"external_id": "archived-import-1",
					"title":       "Imported",
					"author":      "alice",
					"status":      "open",
					"created_at":  "2026-05-01T10:00:00Z",
					"updated_at":  "2026-05-01T10:00:00Z",
				}},
			}},
		{"listIssues", http.MethodGet, "/api/v1/projects/" + pid + "/issues", nil},
		{"listAllIssues_byProject", http.MethodGet, "/api/v1/issues?project_id=" + pid, nil},
		{"pollProjectEvents", http.MethodGet, "/api/v1/projects/" + pid + "/events", nil},
		{"digestProject", http.MethodGet,
			"/api/v1/projects/" + pid + "/digest?since=" +
				url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)), nil},
		{"readyIssues", http.MethodGet, "/api/v1/projects/" + pid + "/ready", nil},
		{"listLabels", http.MethodGet, "/api/v1/projects/" + pid + "/labels", nil},
		{"searchIssues", http.MethodGet, "/api/v1/projects/" + pid + "/search?q=anything", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, bs := doReq(t, ts, tc.method, tc.path, tc.body, nil)
			assertAPIError(t, resp.StatusCode, bs, http.StatusNotFound, "project_not_found")
		})
	}
}

// TestArchivedProject_IssueScopedHandlersReturn404 covers every project-
// scoped handler that operates on a specific issue. After force-archiving
// the parent project, the issue's own row still exists (archival is
// soft-delete on the project, not its issues), but every surface API
// must hide it behind project_not_found.
//
// The destructive endpoints (delete / restore / purge) are deliberately
// included — once the project is archived, an admin cleanup flow should
// restore the project first; per-issue mutations leak archived state
// otherwise.
func TestArchivedProject_IssueScopedHandlersReturn404(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	pid := strconv.FormatInt(projectID, 10)

	// Seed one issue we can target. The issue's number is guaranteed to be
	// 1 since the project is fresh.
	resp, bs := postJSON(t, ts, "/api/v1/projects/"+pid+"/issues",
		map[string]any{"actor": "tester", "title": "to be archived", "body": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	const issueNumber = 1

	// Force-archive: open issue exists, so plain RemoveProject would be
	// refused by the open-issues guard.
	archiveProject(t, h, projectID, true)

	num := strconv.Itoa(issueNumber)
	cases := []struct {
		name    string
		method  string
		path    string
		body    any
		headers map[string]string
	}{
		{"showIssue", http.MethodGet,
			"/api/v1/projects/" + pid + "/issues/" + num, nil, nil},
		{"editIssue", http.MethodPatch,
			"/api/v1/projects/" + pid + "/issues/" + num,
			map[string]any{"actor": "tester", "title": "renamed"}, nil},
		{"createComment", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/comments",
			map[string]any{"actor": "tester", "body": "hi"}, nil},
		{"closeIssue", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/actions/close",
			map[string]any{
				"actor":    "tester",
				"reason":   "done",
				"message":  "Closed after fixing the regression and verifying the full test matrix passes.",
				"evidence": []map[string]any{{"type": "commit", "sha": "abc1234"}},
			}, nil},
		{"reopenIssue", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/actions/reopen",
			map[string]any{"actor": "tester"}, nil},
		{"assignIssue", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/actions/assign",
			map[string]any{"actor": "tester", "owner": "alice"}, nil},
		{"unassignIssue", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/actions/unassign",
			map[string]any{"actor": "tester"}, nil},
		{"addLabel", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/labels",
			map[string]any{"actor": "tester", "label": "bug"}, nil},
		{"removeLabel", http.MethodDelete,
			"/api/v1/projects/" + pid + "/issues/" + num + "/labels/bug?actor=tester",
			nil, nil},
		{"createLink", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/links",
			map[string]any{"actor": "tester", "type": "related", "to_ref": "abcd"}, nil},
		{"deleteLink", http.MethodDelete,
			"/api/v1/projects/" + pid + "/issues/" + num + "/links/1?actor=tester",
			nil, nil},
		{"deleteIssue", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/actions/delete",
			map[string]any{"actor": "tester"},
			map[string]string{"X-Kata-Confirm": "DELETE #" + num}},
		{"restoreIssue", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/actions/restore",
			map[string]any{"actor": "tester"}, nil},
		{"purgeIssue", http.MethodPost,
			"/api/v1/projects/" + pid + "/issues/" + num + "/actions/purge",
			map[string]any{"actor": "tester"},
			map[string]string{"X-Kata-Confirm": "PURGE #" + num}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, bs := doReq(t, ts, tc.method, tc.path, tc.body, tc.headers)
			assertAPIError(t, resp.StatusCode, bs, http.StatusNotFound, "project_not_found")
		})
	}
}

// TestArchivedProject_ShowIssueByUIDReturns404 covers the /api/v1/issues/{uid}
// path that does not carry project_id in the URL. The handler resolves the
// issue first, then must reject when the parent project is archived.
func TestArchivedProject_ShowIssueByUIDReturns404(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)

	pid := strconv.FormatInt(projectID, 10)
	resp, bs := postJSON(t, ts, "/api/v1/projects/"+pid+"/issues",
		map[string]any{"actor": "tester", "title": "uid-show-target", "body": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	var created struct {
		Issue struct {
			UID string `json:"uid"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	require.NotEmpty(t, created.Issue.UID)

	archiveProject(t, h, projectID, true)

	resp2, err := http.Get(ts.URL + "/api/v1/issues/" + created.Issue.UID) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	bs2, _ := io.ReadAll(resp2.Body)
	assertAPIError(t, resp2.StatusCode, bs2, http.StatusNotFound, "project_not_found")
}

// TestListAllIssues_NegativeProjectIDReturns400 pins that
// GET /api/v1/issues?project_id=-1 is a validation error rather than
// silently falling through the > 0 guard and returning all projects'
// issues.
func TestListAllIssues_NegativeProjectIDReturns400(t *testing.T) {
	h, _ := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)

	resp, err := http.Get(ts.URL + "/api/v1/issues?project_id=-1") //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, _ := io.ReadAll(resp.Body)

	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
	assert.Contains(t, string(bs), "project_id", string(bs))
}

// TestArchivedProject_SSEStreamRejects covers the SSE handler path that
// is wired through the raw mux instead of huma. An archived project_id
// must short-circuit to project_not_found before opening a stream.
func TestArchivedProject_SSEStreamRejects(t *testing.T) {
	h, projectID := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)

	archiveProject(t, h, projectID, false)

	pid := strconv.FormatInt(projectID, 10)
	resp, bs := doReq(t, ts, http.MethodGet,
		"/api/v1/events/stream?project_id="+pid, nil,
		map[string]string{"Accept": "text/event-stream"})

	assertAPIError(t, resp.StatusCode, bs, http.StatusNotFound, "project_not_found")
}

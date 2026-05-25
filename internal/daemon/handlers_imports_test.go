package daemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestImportEndpoint_CreatesAndReimports(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Imported",
			"body":        "body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"labels":      []string{"source:beads", "beads-id:beads-1"},
			"comments": []map[string]any{{
				"external_id": "c1",
				"author":      "alice",
				"body":        "comment",
				"created_at":  "2026-05-01T10:01:00Z",
			}},
		}},
	}
	var out struct {
		Source   string   `json:"source"`
		Created  int      `json:"created"`
		Comments int      `json:"comments"`
		Errors   []string `json:"errors"`
		Items    []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &out)
	assert.Equal(t, "beads", out.Source)
	assert.Equal(t, 1, out.Created)
	assert.Equal(t, 1, out.Comments)
	assert.NotNil(t, out.Errors, "success response should emit errors: []")
	assert.Empty(t, out.Errors)
	require.Len(t, out.Items, 1)

	var second struct {
		Created   int `json:"created"`
		Unchanged int `json:"unchanged"`
		Comments  int `json:"comments"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &second)
	assert.Equal(t, 0, second.Created)
	assert.Equal(t, 1, second.Unchanged)
	assert.Equal(t, 0, second.Comments)

	issue, err := env.DB.IssueByShortID(context.Background(), pid, out.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	var commentCount int
	err = env.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM comments WHERE issue_id = ?`, issue.ID).Scan(&commentCount)
	require.NoError(t, err)
	assert.Equal(t, 1, commentCount, "reimport should not duplicate mapped comments")
}

func TestImportEndpoint_BroadcastsAndEnqueuesHookEvents(t *testing.T) {
	sink := &recordingSink{}
	bcast := daemon.NewEventBroadcaster()
	h, pid := bootstrapProject(t, withHooksSink(sink), withBroadcaster(bcast))
	ts := h.ts.(*httptest.Server)
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: pid})
	defer sub.Unsub()

	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Imported",
			"body":        "body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
			"comments": []map[string]any{{
				"external_id": "c1",
				"author":      "alice",
				"body":        "comment",
				"created_at":  "2026-05-01T10:01:00Z",
			}},
		}},
	}

	resp, bs := postJSON(t, ts, importEndpointPath(pid), body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "import status: %s", string(bs))

	broadcastTypes := []string{
		receiveMsg(t, sub.Ch, time.Second, "issue created broadcast").Event.Type,
		receiveMsg(t, sub.Ch, time.Second, "comment broadcast").Event.Type,
	}
	assert.Equal(t, []string{"issue.created", "issue.commented"}, broadcastTypes)

	captured := sink.snapshot()
	require.Len(t, captured, 2)
	hookTypes := []string{captured[0].Type, captured[1].Type}
	assert.Equal(t, broadcastTypes, hookTypes)
}

func TestImportEndpoint_SourceNewerUpdatesIssue(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-1",
			"title":       "Old title",
			"body":        "old body",
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	var first struct {
		Created int `json:"created"`
		Items   []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &first)
	require.Equal(t, 1, first.Created)
	require.Len(t, first.Items, 1)

	body["items"] = []map[string]any{{
		"external_id":   "beads-1",
		"title":         "New title",
		"body":          "new body",
		"author":        "alice",
		"status":        "closed",
		"closed_reason": "done",
		"created_at":    "2026-05-01T10:00:00Z",
		"updated_at":    "2026-05-01T11:00:00Z",
		"closed_at":     "2026-05-01T11:00:00Z",
	}}
	var second struct {
		Updated int `json:"updated"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &second)
	assert.Equal(t, 1, second.Updated)

	issue, err := env.DB.IssueByShortID(context.Background(), pid, first.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "New title", issue.Title)
	assert.Equal(t, "new body", issue.Body)
	assert.Equal(t, "closed", issue.Status)
	require.NotNil(t, issue.ClosedReason)
	assert.Equal(t, "done", *issue.ClosedReason)
}

func TestImportEndpoint_PriorityRoundtrips(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{importEndpointItem(map[string]any{
			"external_id": "beads-prio",
			"priority":    1,
		})},
	}
	var out struct {
		Items []struct {
			IssueShortID string `json:"issue_short_id"`
		} `json:"items"`
	}
	envPostJSON(t, env, importEndpointPath(pid), body, &out)
	require.Len(t, out.Items, 1)
	issue, err := env.DB.IssueByShortID(context.Background(), pid, out.Items[0].IssueShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotNil(t, issue.Priority)
	assert.Equal(t, int64(1), *issue.Priority)
}

func TestImportEndpoint_PriorityOutOfRangeIsValidation(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	body := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{importEndpointItem(map[string]any{
			"external_id": "beads-bad-prio",
			"priority":    9,
		})},
	}
	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), body, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
}

func TestImportEndpoint_RejectsBlankActor(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{name: "missing", body: map[string]any{"source": "beads", "items": []map[string]any{}}},
		{name: "blank", body: map[string]any{"actor": "   ", "source": "beads", "items": []map[string]any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), tc.body, nil)
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
			assert.Contains(t, string(body), "actor")
		})
	}
}

func TestImportEndpoint_InvalidImportMapsToValidation(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	for _, tc := range []struct {
		name string
		item map[string]any
	}{
		{
			name: "status",
			item: importEndpointItem(map[string]any{"status": "bad"}),
		},
		{
			name: "label",
			item: importEndpointItem(map[string]any{"labels": []string{"BadCase"}}),
		},
		{
			name: "empty closed reason",
			item: importEndpointItem(map[string]any{
				"status":        "closed",
				"closed_reason": "",
				"closed_at":     "2026-05-01T10:00:00Z",
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), map[string]any{
				"actor":  "importer",
				"source": "beads",
				"items":  []map[string]any{tc.item},
			}, nil)
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		})
	}
}

func importEndpointItem(overrides map[string]any) map[string]any {
	item := map[string]any{
		"external_id": "beads-invalid",
		"title":       "Imported",
		"body":        "body",
		"author":      "alice",
		"status":      "open",
		"created_at":  "2026-05-01T10:00:00Z",
		"updated_at":  "2026-05-01T10:00:00Z",
	}
	for k, v := range overrides {
		item[k] = v
	}
	return item
}

func importEndpointPath(projectID int64) string {
	return "/api/v1/projects/" + strconv.FormatInt(projectID, 10) + "/imports"
}

func createImportTestProject(t *testing.T, env *testenv.Env, _ string, name string) db.Project {
	t.Helper()
	p, err := env.DB.CreateProject(context.Background(), name)
	require.NoError(t, err)
	return p
}

// TestImportEndpoint_AcceptsLargeBody guards against regressing to huma's
// default 1 MiB MaxBodyBytes — large beads imports (a few hundred issues with
// comments) easily exceed 1 MiB, and the importer does all of bd export + per-
// issue bd comments before POSTing, so a 413 wastes minutes of upstream work.
func TestImportEndpoint_AcceptsLargeBody(t *testing.T) {
	env := testenv.New(t)
	pid := createImportTestProject(t, env, "github.com/wesm/kata", "kata").ID
	// 1.5 MiB body field puts the serialized request comfortably above
	// huma's 1 MiB default but well under any reasonable per-endpoint cap.
	largeBody := strings.Repeat("x", 1500000)
	req := map[string]any{
		"actor":  "importer",
		"source": "beads",
		"items": []map[string]any{{
			"external_id": "beads-large",
			"title":       "Imported",
			"body":        largeBody,
			"author":      "alice",
			"status":      "open",
			"created_at":  "2026-05-01T10:00:00Z",
			"updated_at":  "2026-05-01T10:00:00Z",
		}},
	}
	resp, raw := envDoRaw(t, env, http.MethodPost, importEndpointPath(pid), req, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"large import body rejected (status=%d body=%s)", resp.StatusCode, string(raw))
}

package mcpserver

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	kataclient "go.kenn.io/kata/pkg/client"
)

// Imports can also make a title or the request itself larger than the inbox's
// response bound. Such a candidate is named instead of listed, and pagination
// still reaches the requests after it.
func TestInboxPagesPastOversizedImportedRequests(t *testing.T) {
	session, store := newCoordinationSession(t)
	project, err := store.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	huge := strings.Repeat("a", 9<<20)
	request := func(message string) map[string]jsontext.Value {
		value, err := json.Marshal(map[string]string{"from": "example-agent", "message": message})
		require.NoError(t, err)
		return map[string]jsontext.Value{notification.MetadataKey("reader"): value}
	}
	var listed string
	var oversized []string
	for i, tc := range []struct {
		title   string
		message string
	}{{"Readable", "Review"}, {huge, "Review"}, {"Huge request", huge}} {
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: tc.title, Author: "importer"})
		require.NoError(t, err, i)
		_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "importer", Patch: request(tc.message)})
		require.NoError(t, err, i)
		if i == 0 {
			listed = issue.UID
		} else {
			oversized = append(oversized, project.Name+"#"+issue.ShortID)
		}
	}

	var got, named []string
	cursor := ""
	for range 6 {
		arguments := map[string]any{"for": "reader", "limit": 1}
		if cursor != "" {
			arguments["cursor"] = cursor
		}
		page := callAdministrationTool(t, session, "kata.inbox", arguments)
		for _, raw := range page["requests"].([]any) {
			got = append(got, raw.(map[string]any)["issue_uid"].(string))
		}
		for _, raw := range asSlice(page["oversized"]) {
			named = append(named, raw.(string))
		}
		if cursor, _ = page["next_cursor"].(string); cursor == "" {
			break
		}
	}
	require.Equal(t, []string{listed}, got)
	require.ElementsMatch(t, oversized, named)
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// An imported issue can carry a body larger than the inbox's response bound.
// Its request must still be listed, and pagination must reach the next one.
func TestInboxPagesPastAnIssueWithAHugeBody(t *testing.T) {
	session, store := newCoordinationSession(t)
	project, err := store.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	request := map[string]any{notification.MetadataKey("reader"): map[string]any{"from": "example-agent", "message": "Review"}}
	var want []string
	for i, body := range []string{"short", strings.Repeat("a", 9<<20)} {
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprint("Imported ", i), Body: body, Author: "importer"})
		require.NoError(t, err)
		callAdministrationTool(t, session, "kata.set_metadata", map[string]any{"ref": issue.ShortID, "patch": request})
		want = append(want, issue.UID)
	}

	var got []string
	cursor := ""
	for range 4 {
		arguments := map[string]any{"for": "reader", "limit": 1}
		if cursor != "" {
			arguments["cursor"] = cursor
		}
		page := callAdministrationTool(t, session, "kata.inbox", arguments)
		for _, raw := range page["requests"].([]any) {
			got = append(got, raw.(map[string]any)["issue_uid"].(string))
		}
		if cursor, _ = page["next_cursor"].(string); cursor == "" {
			break
		}
	}
	require.ElementsMatch(t, want, got)
}

func TestInboxPaginationPreservesEmptyCandidatePage(t *testing.T) {
	session, _ := newCoordinationSession(t)
	for i := range 25 {
		created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": fmt.Sprint("Attention issue ", i), "idempotency_key": fmt.Sprint("page-", i)})
		ref := created["issue"].(map[string]any)["ref"].(string)
		from := "example-agent"
		if i == 24 {
			from = ""
		}
		callAdministrationTool(t, session, "kata.set_metadata", map[string]any{"ref": ref, "patch": map[string]any{notification.MetadataKey("reader/worker"): map[string]any{"from": from, "message": "Review"}}})
	}
	first := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "reader/worker", "limit": 1})
	require.Empty(t, first["requests"])
	cursor, ok := first["next_cursor"].(string)
	require.True(t, ok, "an empty filtered page must preserve the candidate continuation")
	seen := map[string]bool{}
	for pages := 0; cursor != ""; pages++ {
		require.Less(t, pages, 30)
		page := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "reader/worker", "limit": 3, "cursor": cursor})
		for _, raw := range page["requests"].([]any) {
			item := raw.(map[string]any)
			uid := item["issue_uid"].(string)
			require.NotEmpty(t, item["project_uid"])
			require.False(t, seen[uid])
			seen[uid] = true
		}
		cursor, _ = page["next_cursor"].(string)
	}
	require.Len(t, seen, 24)
}

func TestInboxCursorScopeMismatch(t *testing.T) {
	session, _ := newCoordinationSession(t)
	for i := range 2 {
		created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": fmt.Sprint("Request ", i), "idempotency_key": fmt.Sprint("scope-", i)})
		ref := created["issue"].(map[string]any)["ref"].(string)
		callAdministrationTool(t, session, "kata.set_metadata", map[string]any{"ref": ref, "patch": map[string]any{notification.MetadataKey("reader"): map[string]any{"from": "example-agent", "message": "Review"}}})
	}
	first := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "reader", "limit": 1})
	cursor, ok := first["next_cursor"].(string)
	require.True(t, ok)
	callToolError(t, session, "kata.inbox", map[string]any{"for": "reader/other", "limit": 1, "cursor": cursor})
}

func TestInboxResponseBoundBeforeDecode(t *testing.T) {
	var limits []int
	oversized := strings.Repeat(" ", 8<<20) + "invalid json that must not be decoded"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/health":
			_, _ = io.WriteString(w, `{"api_schema_version":"0.27.0"}`)
		case strings.HasSuffix(r.URL.Path, "/issues"):
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			limits = append(limits, limit)
			if limit > 1 {
				_, _ = io.WriteString(w, oversized)
				return
			}
			_, _ = io.WriteString(w, `{"issues":[],"complete":false,"next_cursor":"next-candidate"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: client, Scope: scope}}
	_, out, err := h.inbox(t.Context(), nil, InboxInput{For: "reader", Limit: 4})
	require.NoError(t, err, "oversized candidate pages retry before JSON decoding")
	require.Equal(t, []int{4, 2, 1}, limits)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"next_cursor":"next-candidate"`)
}

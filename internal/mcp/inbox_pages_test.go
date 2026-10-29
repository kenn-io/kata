package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestInboxPaginationPreservesEmptyCandidatePage(t *testing.T) {
	session, _ := newCoordinationSession(t)
	for i := range 25 {
		created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": "Attention issue", "idempotency_key": fmt.Sprint("page-", i)})
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
		created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": "Request", "idempotency_key": fmt.Sprint("scope-", i)})
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

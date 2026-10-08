package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestMCPPlainCommentOmitsForceForOlderDaemon(t *testing.T) {
	checkMCPOptionalForce(t, "Plain comment", false)
}

func FuzzMCPCommentOptionalForce(f *testing.F) {
	f.Add("Plain comment", false)
	f.Add("Answer", true)
	f.Fuzz(func(t *testing.T, body string, force bool) {
		if !utf8.ValidString(body) || strings.TrimSpace(body) == "" || len(body) > 4096 {
			return
		}
		checkMCPOptionalForce(t, body, force)
	})
}

func checkMCPOptionalForce(t *testing.T, body string, force bool) {
	t.Helper()
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			version := "0.25.0"
			if force {
				version = "0.26.0"
			}
			writeJSON(w, map[string]any{"api_schema_version": version})
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "example-project")}})
		case "/api/v1/projects/1/issues/aaaa/comments":
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			value, present := payload["force"]
			if !force && present {
				http.Error(w, "unknown field force", http.StatusUnprocessableEntity)
				return
			}
			require.Equal(t, force, present)
			if force {
				require.Equal(t, true, value)
			}
			require.Equal(t, body, payload["body"])
			writeJSON(w, map[string]any{"issue": map[string]any{"uid": "issue"}, "comment": map[string]any{"uid": "comment", "body": body}, "changed": true})
		default:
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "example-project"}})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "worker"}}
	input := CommentInput{Ref: "example-project#aaaa", Body: body, Force: force, IdempotencyKey: "comment-key"}
	if force {
		input.ReplyTo, input.Kind = "c:aaaaaa", "reply"
	}
	_, out, err := h.comment(t.Context(), nil, input)
	require.NoError(t, err)
	require.True(t, out.Changed)
}

func TestShowCommentAnnotationsAndRootLimit(t *testing.T) {
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			writeJSON(w, map[string]any{"api_schema_version": "0.26.0"})
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "example-project")}})
		case "/api/v1/projects/1/issues/aaaa":
			require.Equal(t, "c:aaaaaa", r.URL.Query().Get("thread"))
			writeJSON(w, map[string]any{"issue": map[string]any{"project_id": 1, "uid": "issue", "short_id": "aaaa"}, "comments_truncated": true, "comments": []any{
				map[string]any{"issue_uid": "issue", "issue_short_id": "aaaa", "uid": "01AAAAAAAAAAAAAAAAAAAAAAAA", "handle": "c:aaaaaa", "author": "finder", "body": "Finding", "created_at": "2030-01-01T00:00:00Z", "backlinks": []any{map[string]any{"uid": "01BBBBBBBBBBBBBBBBBBBBBBBB", "issue_uid": "issue", "kind": "reply", "handle": "c:bbbbbb", "project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA", "body": "Response evidence", "created_at": "2030-01-02T00:00:00Z", "target_edited": true}}},
				map[string]any{"uid": "01BBBBBBBBBBBBBBBBBBBBBBBB", "handle": "c:bbbbbb", "author": "worker", "body": "Answer", "created_at": "2030-01-02T00:00:00Z"},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "example-project"}})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "worker"}}
	_, out, err := h.show(t.Context(), nil, ShowInput{Ref: "example-project#aaaa", Thread: "c:aaaaaa", CommentLimit: 1})
	require.NoError(t, err)
	require.True(t, out.CommentsTruncated)
	require.Len(t, out.Issue.Comments, 1)
	require.Equal(t, "c:aaaaaa", out.Issue.Comments[0].Handle)
	require.Len(t, out.Issue.Comments[0].Backlinks, 1)
	require.Equal(t, "Response evidence", out.Issue.Comments[0].Backlinks[0].Body)
	require.Equal(t, "2030-01-02T00:00:00Z", out.Issue.Comments[0].Backlinks[0].CreatedAt.Format(time.RFC3339))
	require.True(t, out.Issue.Comments[0].Backlinks[0].TargetEdited)
}

func TestMCPCommentTypedPayload(t *testing.T) {
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			writeJSON(w, map[string]any{"api_schema_version": "0.26.0"})
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "example-project")}})
		case "/api/v1/projects/1/issues/aaaa/comments":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "c:aaaaaa", body["reply_to"])
			require.Equal(t, "reply", body["kind"])
			require.Equal(t, true, body["force"])
			require.Equal(t, "reply-key", r.Header.Get("Idempotency-Key"))
			writeJSON(w, map[string]any{"issue": map[string]any{"uid": "issue"}, "comment": map[string]any{"uid": "comment", "body": "Answer"}, "changed": true})
		default:
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "example-project"}})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "worker"}}
	_, out, err := h.comment(t.Context(), nil, CommentInput{Ref: "example-project#aaaa", Body: "Answer", ReplyTo: "c:aaaaaa", Kind: "reply", Force: true, IdempotencyKey: "reply-key"})
	require.NoError(t, err)
	require.True(t, out.Changed)
}

func TestMCPCommentLinksRespectStartupProjectUIDs(t *testing.T) {
	const allowed = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	const moved = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, allowed, "example-project"), projectJSON(2, moved, "other-project")}})
		case "/api/v1/projects/1/issues/aaaa":
			writeJSON(w, map[string]any{"issue": map[string]any{"uid": "issue", "project_id": 1}, "comments": []any{map[string]any{"uid": "comment", "handle": "c:aaaaaa", "body": "Answer", "author": "worker", "created_at": "2030-01-01T00:00:00Z", "reply": map[string]any{"uid": "target", "kind": "reply", "project_uid": moved, "issue_uid": "other-issue", "status": "moved"}, "backlinks": []any{map[string]any{"uid": "outside", "kind": "confirm", "project_uid": moved}, map[string]any{"uid": "inside", "kind": "confirm", "project_uid": allowed}}}}})
		default:
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: allowed, Name: "example-project"}})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "worker"}}
	_, out, err := h.show(t.Context(), nil, ShowInput{Ref: "example-project#aaaa"})
	require.NoError(t, err)
	require.Len(t, out.Issue.Comments, 1)
	require.Nil(t, out.Issue.Comments[0].Reply)
	require.Len(t, out.Issue.Comments[0].Backlinks, 1)
	require.Equal(t, "inside", out.Issue.Comments[0].Backlinks[0].UID)
}

func TestMCPUnavailableReplyRetainsStatusWithoutIdentity(t *testing.T) {
	const allowed = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, allowed, "example-project")}})
		case "/api/v1/projects/1/issues/aaaa":
			writeJSON(w, map[string]any{"issue": map[string]any{"uid": "source", "project_id": 1}, "comments": []any{map[string]any{"uid": "reply", "author": "worker", "body": "Answer", "created_at": "2030-01-01T00:00:00Z", "reply": map[string]any{"uid": "target", "kind": "reply", "status": "removed"}}}})
		default:
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: allowed, Name: "example-project"}})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "worker"}}
	_, out, err := h.show(t.Context(), nil, ShowInput{Ref: "example-project#aaaa"})
	require.NoError(t, err)
	require.NotNil(t, out.Issue.Comments[0].Reply, "an authorized removed target must retain its reply status")
	require.Equal(t, "removed", out.Issue.Comments[0].Reply.Status)
	require.Equal(t, "reply", out.Issue.Comments[0].Reply.Kind)
	require.Empty(t, out.Issue.Comments[0].Reply.UID)
}

func TestMCPThreadCapRetainsSameIssueRoot(t *testing.T) {
	const allowed = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	const rootUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			writeJSON(w, map[string]any{"api_schema_version": "0.26.0"})
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, allowed, "example-project")}})
		case "/api/v1/projects/1/issues/aaaa":
			writeJSON(w, map[string]any{"issue": map[string]any{"uid": "root-issue", "project_id": 1}, "comments": []any{
				map[string]any{"uid": "01BBBBBBBBBBBBBBBBBBAAAAAA", "issue_uid": "other-issue", "issue_short_id": "bbbb", "body": "Old-clock descendant", "author": "worker", "created_at": "2030-01-01T00:00:00Z"},
				map[string]any{"uid": rootUID, "issue_uid": "root-issue", "issue_short_id": "aaaa", "body": "Root", "author": "worker", "created_at": "2030-01-02T00:00:00Z"},
				map[string]any{"uid": "01CCCCCCCCCCCCCCCCCCCCCCCC", "issue_uid": "other-issue", "issue_short_id": "bbbb", "body": "Descendant", "author": "worker", "created_at": "2030-01-03T00:00:00Z"},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: allowed, Name: "example-project"}})
	require.NoError(t, err)
	h := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "worker"}}
	_, out, err := h.show(t.Context(), nil, ShowInput{Ref: "example-project#aaaa", Thread: "c:aaaaaa", CommentLimit: 1})
	require.NoError(t, err)
	require.Equal(t, rootUID, out.Issue.Comments[0].UID, "c: root resolution is restricted to the shown issue")
}

func TestMCPReplyKindSchemasUseCanonicalKinds(t *testing.T) {
	session := connectTestServer(t)
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range result.Tools {
		if tool.Name != "kata.comment" && tool.Name != "kata.show" {
			continue
		}
		properties := schemaObject(t, tool.InputSchema)["properties"].(map[string]any)
		kind := properties["kind"].(map[string]any)
		require.Equal(t, []any{"reply", "confirm", "refute", "supersede"}, kind["enum"], tool.Name)
	}
}

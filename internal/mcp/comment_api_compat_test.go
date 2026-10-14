package mcpserver

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPCommentFeaturesRejectDaemonsWithoutCommentLinks(t *testing.T) {
	for _, version := range []string{"0.26.0", "0.27.0"} {
		for _, feature := range []string{"reply", "thread"} {
			t.Run(version+"/"+feature, func(t *testing.T) {
				var featureCalls atomic.Int32
				client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/health":
						writeJSON(w, map[string]any{"api_schema_version": version})
					case "/api/v1/projects":
						writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "example-project")}})
					case "/api/v1/projects/1/issues/aaaa", "/api/v1/projects/1/issues/aaaa/comments":
						featureCalls.Add(1)
						writeJSON(w, map[string]any{"issue": map[string]any{"uid": "issue", "project_id": 1}, "comments": []any{}, "changed": true})
					default:
						http.NotFound(w, r)
					}
				})
				scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "example-project"}})
				require.NoError(t, err)
				h := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "worker"}}
				if feature == "reply" {
					_, _, err = h.comment(t.Context(), nil, CommentInput{Ref: "example-project#aaaa", Body: "Answer", ReplyTo: "c:aaaaaa", Kind: "reply", IdempotencyKey: "reply-key"})
				} else {
					_, _, err = h.show(t.Context(), nil, ShowInput{Ref: "example-project#aaaa", Thread: "c:aaaaaa"})
				}
				require.ErrorContains(t, err, "upgrade the daemon")
				assert.Zero(t, featureCalls.Load(), "reject unsupported comment requests before sending them")
			})
		}
	}
}

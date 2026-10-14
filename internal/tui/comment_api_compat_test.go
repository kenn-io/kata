package tui

import (
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientCommentReplyRequiresCommentLinksAPI(t *testing.T) {
	for _, tc := range []struct {
		version string
		allowed bool
	}{
		{"0.26.0", false},
		{"0.27.0", false},
		{"0.28.0", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			var commentCalls atomic.Int32
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					respondJSON(t, w, map[string]any{"ok": true, "api_schema_version": tc.version})
				case "/api/v1/projects/1/issues/abc1/comments":
					commentCalls.Add(1)
					var payload map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &payload))
					assert.Equal(t, "c:abcdef", payload["reply_to"])
					assert.Equal(t, "reply", payload["kind"])
					assert.Equal(t, "Answer", payload["body"])
					assert.Equal(t, "reply-key", r.Header.Get("Idempotency-Key"))
					respondJSON(t, w, map[string]any{"changed": true})
				default:
					http.NotFound(w, r)
				}
			})
			response, err := client.AddCommentReply(t.Context(), 1, "abc1", "Answer", "worker", "c:abcdef", "reply", false, "reply-key")
			if tc.allowed {
				require.NoError(t, err)
				assert.True(t, response.Changed)
				assert.Equal(t, int32(1), commentCalls.Load())
			} else {
				require.ErrorContains(t, err, "upgrade the daemon")
				assert.Zero(t, commentCalls.Load(), "reject unsupported typed replies before sending them")
			}
		})
	}
}

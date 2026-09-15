package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
)

func TestCommentEndpoint_TeammateWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"populated", "reviewer-7", true}, {"empty", "", true}, {"null", nil, true},
		{"space", " reviewer-7", false}, {"slash", "actor/child", false}, {"unicode", "réviewer", false}, {"long", strings.Repeat("a", 65), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ts, pid, id := bootstrapProjectWithIssue(t)
			resp, bs := postJSON(t, ts, issueURL(pid, id, "comments"), map[string]any{"actor": "coordinator", "body": "check retries", "teammate": tc.value})
			if !tc.valid {
				require.GreaterOrEqual(t, resp.StatusCode, 400, string(bs))
				comments, err := h.DB().CommentsByIssue(t.Context(), id)
				require.NoError(t, err)
				require.Empty(t, comments)
				return
			}
			require.Equal(t, 200, resp.StatusCode, string(bs))
			var got struct {
				Comment map[string]any `json:"comment"`
			}
			require.NoError(t, json.Unmarshal(bs, &got))
			require.Equal(t, "coordinator", got.Comment["author"])
			if tc.value != nil && tc.value != "" {
				require.Equal(t, tc.value, got.Comment["teammate"])
			} else {
				require.NotContains(t, got.Comment, "teammate")
			}
			comments, err := h.DB().CommentsByIssue(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			saved, err := json.Marshal(comments[0])
			require.NoError(t, err)
			if tc.value != nil && tc.value != "" {
				require.Contains(t, string(saved), `"teammate":"reviewer-7"`)
			}
		})
	}
}

func TestCommentEndpoint_TeammateRetryMismatch(t *testing.T) {
	_, ts, pid, id := bootstrapProjectWithIssue(t)
	headers := map[string]string{"Idempotency-Key": "teammate-key"}
	body := map[string]any{"actor": "coordinator", "body": "check retries", "teammate": "reviewer-7"}
	requireOK(t, postWithHeader(t, ts, issueURL(pid, id, "comments"), headers, body))
	requireOK(t, postWithHeader(t, ts, issueURL(pid, id, "comments"), headers, body))
	for _, value := range []any{"implementer-3", "", nil} {
		body["teammate"] = value
		got := postWithHeader(t, ts, issueURL(pid, id, "comments"), headers, body)
		require.Equal(t, 409, got.status, string(got.body))
	}
}

func TestCommentEndpoint_TeammateKeepsAuthenticatedActor(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		t.Run(fmt.Sprint(readonly), func(t *testing.T) {
			h, _, pid, id := bootstrapProjectWithIssue(t)
			server := daemon.NewServer(daemon.ServerConfig{DB: h.DB(), StartedAt: time.Now()})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				kind := daemon.PrincipalDBToken
				if readonly {
					kind = daemon.PrincipalBootstrap
				}
				ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: kind, Actor: "coordinator"})
				server.Handler().ServeHTTP(w, r.WithContext(ctx))
			}))
			t.Cleanup(ts.Close)
			resp, bs := postJSON(t, ts, issueURL(pid, id, "comments"), map[string]any{"actor": "forged", "body": "check retries", "teammate": "reviewer-7"})
			if readonly {
				require.GreaterOrEqual(t, resp.StatusCode, 400)
				return
			}
			require.Equal(t, 200, resp.StatusCode, string(bs))
			var got map[string]any
			require.NoError(t, json.Unmarshal(bs, &got))
			comment := got["comment"].(map[string]any)
			require.Equal(t, "coordinator", comment["author"])
			require.Equal(t, "reviewer-7", comment["teammate"])
			issue, err := h.DB().IssueByID(t.Context(), id)
			require.NoError(t, err)
			require.Nil(t, issue.Owner)
		})
	}
}

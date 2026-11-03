package daemon_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestRequestDisplayContribution(t *testing.T) {
	h, ts, pid, ref := bootstrapProjectWithIssue(t)
	path := issueURL(pid, ref, "comments")
	body := map[string]any{"actor": "coordinator", "body": "progress"}
	first := postWithHeader(t, ts, path, map[string]string{"X-Kata-Teammate": "worker-1", "Idempotency-Key": "display-request"}, body)
	requireOK(t, first)
	var initial api.CommentResponse
	require.NoError(t, json.Unmarshal(first.body, &initial.Body))
	require.Contains(t, initial.Body.Event.Payload, `"attributed_teammate":"worker-1"`)
	require.NotContains(t, string(first.body), `"teammate":"worker-1"`, "display must not become comment identity")
	replay := postWithHeader(t, ts, path, map[string]string{"X-Kata-Teammate": "worker-2", "Idempotency-Key": "display-request"}, body)
	requireOK(t, replay)
	var retried api.CommentResponse
	require.NoError(t, json.Unmarshal(replay.body, &retried.Body))
	require.Nil(t, retried.Body.Event, "replay remains a no-op response")
	retained, err := h.DB().EventsByUIDs(t.Context(), pid, []string{initial.Body.Event.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{*initial.Body.Event}, retained)
	invalid := postWithHeader(t, ts, path, map[string]string{"X-Kata-Teammate": "bad/handle"}, body)
	require.Equal(t, 400, invalid.status)
}

func TestRequestContributionAuthenticatesFirst(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("display-test-token"))
	for _, test := range []struct {
		token, handle string
		status        int
	}{
		{"", "bad/handle", http.StatusUnauthorized},
		{"display-test-token", "bad/handle", http.StatusBadRequest},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, env.URL+"/api/v1/projects", strings.NewReader(`{"name":"example-project"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kata-Teammate", test.handle)
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		resp, err := env.HTTP.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, test.status, resp.StatusCode)
	}
}

package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Property 13: the same stamped close survives every public event transport.
func TestEventTeammateEnvelopeParity(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	newScopedTokens(t, env, project, root)
	// Inject the already-supported storage context; request header transport is separate work.
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, Auth: config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true}, StartedAt: time.Now()})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.Handler().ServeHTTP(w, r.WithContext(db.WithEventTeammate(r.Context(), "example-worker")))
	}))
	t.Cleanup(ts.Close)
	env.URL = ts.URL
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	resp, body := envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID, "issues/"+root.ShortID+"/actions/close"),
		map[string]any{"actor": "coordinator", "reason": "done", "message": "Completed the example task with focused checks.", "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}}}, map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var mutation struct {
		Event map[string]any `json:"event"`
	}
	require.NoError(t, json.Unmarshal(body, &mutation))
	require.Equal(t, "example-worker", mutation.Event["teammate"])
	for _, token := range []string{"coordinator-token", "worker-token"} {
		for _, path := range []string{scopedProjectPath(project.ID, "events") + "?issue=" + root.UID + fmt.Sprintf("&after_id=%d", before), fmt.Sprintf("/api/v1/events?after_id=%d", before)} {
			resp, body := envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + token})
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			var page struct {
				Events []map[string]any `json:"events"`
			}
			require.NoError(t, json.Unmarshal(body, &page))
			require.Len(t, page.Events, 1)
			require.Equal(t, "example-worker", page.Events[0]["teammate"], "%s %s", token, path)
		}
		stream := openSSE(t, env, fmt.Sprintf("after_id=%d", before), http.Header{"Authorization": {"Bearer " + token}})
		t.Cleanup(func() { _ = stream.Body.Close() })
		require.Equal(t, http.StatusOK, stream.StatusCode)
		frame, ok := newSSEFramer(stream.Body).Next(t, 2*time.Second)
		require.True(t, ok)
		require.NoError(t, stream.Body.Close())
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(frame.data), &event))
		require.Equal(t, "issue.closed", event["type"])
		require.Equal(t, "example-worker", event["teammate"])
	}
}

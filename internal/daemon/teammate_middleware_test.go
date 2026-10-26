package daemon_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestTeammateHeaderStampsEvents(t *testing.T) {
	const origin = "http://127.0.0.1:27123"
	d := openTestDB(t)
	project, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	auth := config.AuthConfig{Token: "test-token"}
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: origin, InstanceID: "teammate_test", Writable: true, Auth: auth, DB: d.db,
	})
	require.NoError(t, err)
	issued, err := manager.IssueSession(daemon.Principal{Kind: daemon.PrincipalStaticToken}, "/kata")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: d.db, Auth: auth, WebSessions: manager, StartedAt: d.now})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	browser, err := server.HandlerFor(daemon.ListenerPolicy{
		Kind: daemon.ListenerBrowser, Origin: origin, RequireBrowserSession: true,
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, header, want string
		browser, noAuth    bool
		status             int
	}{
		{name: "authenticated", header: "codex", want: "codex", status: 200},
		{name: "absent", status: 200},
		{name: "maximum", header: "0" + strings.Repeat("._-a", 15) + "XYZ", want: "0" + strings.Repeat("._-a", 15) + "XYZ", status: 200},
		{name: "invalid", header: "@bad", status: 400},
		{name: "too long", header: strings.Repeat("a", 65), status: 400},
		{name: "unauthenticated before validation", header: "@bad", noAuth: true, status: 401},
		{name: "browser ignores valid", header: "codex", browser: true, status: 200},
		{name: "browser rejects invalid", header: "@bad", browser: true, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue, _, err := d.db.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Close example task", Author: "coordinator"})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, origin+issueURLRef(project.ID, issue.ShortID, "actions/close"),
				strings.NewReader(`{"actor":"coordinator","reason":"done","message":"Completed the example task with focused checks.","evidence":[{"type":"test","command":"go test ./example"}]}`))
			request.RemoteAddr = "127.0.0.1:40123"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Kata-Teammate", tc.header)
			handler := server.Handler()
			if tc.browser {
				handler = browser
				request.Header.Set("Origin", origin)
				request.Header.Set("X-Kata-Web-Session", issued.Session)
				request.Header.Set("X-Kata-CSRF", issued.CSRF)
				request.AddCookie(manager.Cookie(issued.Cookie))
			} else if !tc.noAuth {
				request.Header.Set("Authorization", "Bearer test-token")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			if tc.status != http.StatusOK {
				if tc.status == http.StatusBadRequest {
					assertAPIError(t, response.Code, response.Body.Bytes(), http.StatusBadRequest, "validation")
				}
				unchanged, err := d.db.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, "open", unchanged.Status)
				return
			}
			var mutation struct {
				Event map[string]any `json:"event"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &mutation))
			require.Equal(t, "coordinator", mutation.Event["actor"])
			if tc.want == "" {
				require.NotContains(t, mutation.Event, "teammate")
			} else {
				require.Equal(t, tc.want, mutation.Event["teammate"])
			}
		})
	}
}

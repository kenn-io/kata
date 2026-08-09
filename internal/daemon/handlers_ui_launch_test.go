package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func TestReadUILaunchTargetReturnsConfiguredBrowserRoute(t *testing.T) {
	dbh := openTestDB(t)
	project, err := dbh.db.CreateProject(t.Context(), "alpha-project")
	require.NoError(t, err)
	issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example task", Author: "user-a",
	})
	require.NoError(t, err)
	manager := newUILaunchTestManager(t, dbh, "https://kata.example")
	ts := startTestServer(t, daemon.ServerConfig{
		DB: dbh.db, StartedAt: dbh.now, WebSessions: manager,
		ActiveWebDaemon: "example-remote",
		WebDaemons: []config.CatalogDaemonConfig{
			{Name: "example-local", Local: true},
			{Name: "example-remote", URL: "https://remote.example"},
		},
	})

	resp, body := getUILaunchTarget(t, ts, issue.UID)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Available bool   `json:"available"`
		URL       string `json:"url"`
		Reason    string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.True(t, out.Available)
	require.Equal(t, "https://kata.example/kata?issue="+issue.UID+"#direct=1", out.URL)
	require.Empty(t, out.Reason)
}

func TestReadUILaunchTargetRejectsUnsafeBrowserOrigins(t *testing.T) {
	unsafeOrigins := []string{
		"https://user@kata.example",
		"https://kata.example/prefix",
		"https://kata.example?tenant=alpha",
		"https://kata.example#fragment",
	}
	for _, origin := range unsafeOrigins {
		t.Run(origin, func(t *testing.T) {
			dbh := openTestDB(t)
			project, err := dbh.db.CreateProject(t.Context(), "alpha-project")
			require.NoError(t, err)
			issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: project.ID, Title: "Example task", Author: "user-a",
			})
			require.NoError(t, err)
			manager := newUILaunchTestManager(t, dbh, origin)
			ts := startTestServer(t, daemon.ServerConfig{
				DB: dbh.db, StartedAt: dbh.now, WebSessions: manager,
			})

			resp, body := getUILaunchTarget(t, ts, issue.UID)

			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			var out struct {
				Available bool   `json:"available"`
				URL       string `json:"url"`
				Reason    string `json:"reason"`
			}
			require.NoError(t, json.Unmarshal(body, &out))
			require.False(t, out.Available)
			require.Empty(t, out.URL)
			require.Equal(t, "browser_origin_unavailable", out.Reason)
		})
	}
}

func TestReadUILaunchTargetUnavailableWithoutBrowserOrigin(t *testing.T) {
	dbh := openTestDB(t)
	project, err := dbh.db.CreateProject(t.Context(), "alpha-project")
	require.NoError(t, err)
	issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example task", Author: "user-a",
	})
	require.NoError(t, err)
	ts := startTestServer(t, daemon.ServerConfig{DB: dbh.db, StartedAt: dbh.now})

	resp, body := getUILaunchTarget(t, ts, issue.UID)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Available bool   `json:"available"`
		URL       string `json:"url"`
		Reason    string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.False(t, out.Available)
	require.Empty(t, out.URL)
	require.Equal(t, "browser_origin_unavailable", out.Reason)
}

func TestReadUILaunchTargetRequiresAnActiveIssue(t *testing.T) {
	dbh := openTestDB(t)
	manager := newUILaunchTestManager(t, dbh, "https://kata.example")
	ts := startTestServer(t, daemon.ServerConfig{
		DB: dbh.db, StartedAt: dbh.now, WebSessions: manager,
	})
	missingUID, err := uid.New()
	require.NoError(t, err)

	resp, body := getUILaunchTarget(t, ts, missingUID)

	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func TestReadUILaunchTargetHidesDeletedAndArchivedIssues(t *testing.T) {
	for _, lifecycle := range []string{"deleted-issue", "archived-project"} {
		t.Run(lifecycle, func(t *testing.T) {
			dbh := openTestDB(t)
			project, err := dbh.db.CreateProject(t.Context(), "alpha-project")
			require.NoError(t, err)
			issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: project.ID, Title: "Example task", Author: "user-a",
			})
			require.NoError(t, err)
			if lifecycle == "deleted-issue" {
				_, _, _, err = dbh.db.SoftDeleteIssue(t.Context(), issue.ID, "user-a")
			} else {
				_, _, err = dbh.db.RemoveProject(t.Context(), db.RemoveProjectParams{
					ProjectID: project.ID, Actor: "user-a", Force: true,
				})
			}
			require.NoError(t, err)
			manager := newUILaunchTestManager(t, dbh, "https://kata.example")
			ts := startTestServer(t, daemon.ServerConfig{
				DB: dbh.db, StartedAt: dbh.now, WebSessions: manager,
			})

			resp, body := getUILaunchTarget(t, ts, issue.UID)

			require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
		})
	}
}

type denyingUILaunchHostAccess struct{}

func (denyingUILaunchHostAccess) Authorize(context.Context, daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
}

func TestReadUILaunchTargetHidesHostAccessDenial(t *testing.T) {
	dbh := openTestDB(t)
	project, err := dbh.db.CreateProject(t.Context(), "alpha-project")
	require.NoError(t, err)
	issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example task", Author: "user-a",
	})
	require.NoError(t, err)
	manager := newUILaunchTestManager(t, dbh, "https://kata.example")
	server := daemon.NewServer(daemon.ServerConfig{
		DB: dbh.db, StartedAt: dbh.now, WebSessions: manager,
		HostAccess: denyingUILaunchHostAccess{},
	})
	ts := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := daemon.WithPrincipal(request.Context(), daemon.Principal{
			Kind: daemon.PrincipalHost, Subject: "forge", Actor: "user-a",
		})
		server.Handler().ServeHTTP(writer, request.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)

	resp, body := getUILaunchTarget(t, ts, issue.UID)

	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}

func newUILaunchTestManager(t *testing.T, dbh testDBHandle, origin string) *daemon.WebSessionManager {
	t.Helper()
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: origin, OriginStable: true, InstanceID: "launch_test",
		Auth: config.AuthConfig{}, DB: dbh.db,
	})
	require.NoError(t, err)
	return manager
}

func getUILaunchTarget(t *testing.T, ts *httptest.Server, issueUID string) (*http.Response, []byte) {
	t.Helper()
	requestURL := ts.URL + "/api/v1/ui/launch-target?issue_uid=" + url.QueryEscape(issueUID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, requestURL, nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

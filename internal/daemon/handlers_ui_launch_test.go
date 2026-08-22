package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

type ambiguousUILaunchStore struct {
	db.Storage
	matches []db.Issue
}

func (s ambiguousUILaunchStore) IssueUIDPrefixMatch(
	context.Context, string, int, db.IncludeDeleted,
) ([]db.Issue, error) {
	return s.matches, nil
}

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

func TestReadUILaunchTargetRequiresExactUID(t *testing.T) {
	dbh := openTestDB(t)
	project, err := dbh.db.CreateProject(t.Context(), "alpha-project")
	require.NoError(t, err)
	issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example task", Author: "user-a",
	})
	require.NoError(t, err)
	manager := newUILaunchTestManager(t, dbh, "https://kata.example")
	leakingMatches := []db.Issue{
		{UID: "01J00000000000000000000001", ShortID: "secret-a", ProjectID: 4242},
		{UID: "01J00000000000000000000002", ShortID: "secret-b", ProjectID: 8484},
	}
	ts := startTestServer(t, daemon.ServerConfig{
		DB:        ambiguousUILaunchStore{Storage: dbh.db, matches: leakingMatches},
		StartedAt: dbh.now, WebSessions: manager,
	})

	padding := strings.Repeat(" ", 9)
	prefixResp, prefixBody := getUILaunchTarget(t, ts, padding+issue.UID[:8]+padding)

	require.Equal(t, http.StatusBadRequest, prefixResp.StatusCode, string(prefixBody))
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(prefixBody, &failure))
	require.Equal(t, "validation", failure.Error.Code)
	for _, match := range leakingMatches {
		require.NotContains(t, string(prefixBody), match.UID)
		require.NotContains(t, string(prefixBody), match.ShortID)
	}
	require.NotContains(t, string(prefixBody), "4242")
	require.NotContains(t, string(prefixBody), "8484")

	fullResp, fullBody := getUILaunchTarget(t, ts, "  "+issue.UID+"  ")
	require.Equal(t, http.StatusOK, fullResp.StatusCode, string(fullBody))
	require.Contains(t, string(fullBody), "/kata?issue="+issue.UID)
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

type scopedUILaunchHostAccess struct {
	deniedProjectID int64
}

func (a scopedUILaunchHostAccess) Authorize(
	_ context.Context,
	request daemon.HostAccessRequest,
) (daemon.HostAccessDecision, error) {
	if slices.Contains(request.Operation.ProjectIDs, a.deniedProjectID) {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostAccessDecision{}, nil
}

func TestReadUILaunchTargetConcealsHostResourceState(t *testing.T) {
	dbh := openTestDB(t)
	activeProject, err := dbh.db.CreateProject(t.Context(), "active-project")
	require.NoError(t, err)
	deniedProject, err := dbh.db.CreateProject(t.Context(), "denied-project")
	require.NoError(t, err)
	archivedProject, err := dbh.db.CreateProject(t.Context(), "archived-project")
	require.NoError(t, err)
	deletedIssue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: activeProject.ID, Title: "Deleted task", Author: "user-a",
	})
	require.NoError(t, err)
	_, _, _, err = dbh.db.SoftDeleteIssue(t.Context(), deletedIssue.ID, "user-a")
	require.NoError(t, err)
	deniedIssue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: deniedProject.ID, Title: "Denied task", Author: "user-a",
	})
	require.NoError(t, err)
	archivedIssue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: archivedProject.ID, Title: "Archived task", Author: "user-a",
	})
	require.NoError(t, err)
	_, _, err = dbh.db.RemoveProject(t.Context(), db.RemoveProjectParams{
		ProjectID: archivedProject.ID, Actor: "user-a", Force: true,
	})
	require.NoError(t, err)
	missingUID, err := uid.New()
	require.NoError(t, err)

	manager := newUILaunchTestManager(t, dbh, "https://kata.example")
	server := daemon.NewServer(daemon.ServerConfig{
		DB: dbh.db, StartedAt: dbh.now, WebSessions: manager,
		HostAccess: scopedUILaunchHostAccess{deniedProjectID: deniedProject.ID},
	})
	ts := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := daemon.WithPrincipal(request.Context(), daemon.Principal{
			Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "user-a",
		})
		server.Handler().ServeHTTP(writer, request.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)

	wantBody := `{"status":404,"error":{"code":"not_found","message":"resource not found"}}`
	for name, issueUID := range map[string]string{
		"missing":      missingUID,
		"deleted":      deletedIssue.UID,
		"archived":     archivedIssue.UID,
		"unauthorized": deniedIssue.UID,
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := getUILaunchTarget(t, ts, issueUID)

			require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
			require.JSONEq(t, wantBody, string(body))
		})
	}
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

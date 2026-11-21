package daemon_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type historicalUIReferenceHostAccess struct {
	deniedProjectID int64
}

func TestUIIssueReferenceProjectSelector(t *testing.T) {
	dbh := openTestDB(t)
	project, err := dbh.db.CreateProject(t.Context(), "example")
	require.NoError(t, err)
	issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Target", Author: "worker"})
	require.NoError(t, err)
	collision, err := dbh.db.CreateProject(t.Context(), project.UID)
	require.NoError(t, err)
	collisionIssue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: collision.ID, Title: "Collision", Author: "worker"})
	require.NoError(t, err)
	server := httptest.NewServer(daemon.NewServer(daemon.ServerConfig{DB: dbh.db, StartedAt: dbh.now}).Handler())
	t.Cleanup(server.Close)
	found := func(scope string, issue db.Issue, projectUID string) string {
		return fmt.Sprintf(`{"scope_project_uid":%q,"issue":{"uid":%q,"project_uid":%q}}`, scope, issue.UID, projectUID)
	}
	for _, tc := range []struct{ name, selector, ref, want string }{
		{"qualified", "", project.Name + "#" + issue.ShortID, fmt.Sprintf(`{"issue":{"uid":%q,"project_uid":%q}}`, issue.UID, project.UID)},
		{"name", project.Name, issue.ShortID, found(project.UID, issue, project.UID)},
		{"uid", collision.UID, collisionIssue.ShortID, found(collision.UID, collisionIssue, collision.UID)},
		{"ambiguous", project.UID, issue.ShortID, ""},
		{"qualified UID-shaped name", "", collision.Name + "#" + collisionIssue.ShortID, fmt.Sprintf(`{"issue":{"uid":%q,"project_uid":%q}}`, collisionIssue.UID, collision.UID)},
		{"qualified with another scope", project.Name, collision.Name + "#" + collisionIssue.ShortID, found(project.UID, collisionIssue, collision.UID)},
		{"unknown", "missing", issue.ShortID, ""},
		{"wrong qualifier", project.Name, "other#" + issue.ShortID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := url.Values{"ref": {tc.ref}}
			if tc.selector != "" {
				query.Set("project", tc.selector)
			}
			status, body := getUIIssueReference(t, server, query)
			if tc.want == "" {
				require.Equal(t, http.StatusNotFound, status, body)
				return
			}
			require.Equal(t, http.StatusOK, status, body)
			require.JSONEq(t, tc.want, body)
		})
	}
}

func TestUIIssueReferenceFullUIDWithViewingProject(t *testing.T) {
	dbh := openTestDB(t)
	scope, err := dbh.db.CreateProject(t.Context(), "scope-project")
	require.NoError(t, err)
	target, err := dbh.db.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "Target", Author: "worker"})
	require.NoError(t, err)
	server := httptest.NewServer(daemon.NewServer(daemon.ServerConfig{DB: dbh.db, StartedAt: dbh.now}).Handler())
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name, selector, scopeHeader string
		status                      int
	}{
		{"both allowed", "project=" + scope.Name, fmt.Sprintf(`["%s","%s"]`, scope.UID, target.UID), http.StatusOK},
		{"target denied", "project=" + scope.Name, fmt.Sprintf(`["%s"]`, scope.UID), http.StatusNotFound},
		{"numeric project restriction", fmt.Sprintf("project_id=%d", scope.ID), "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/ui/issue-reference?"+tc.selector+"&ref="+issue.UID, nil)
			require.NoError(t, err)
			if tc.scopeHeader != "" {
				req.Header.Set("X-Kata-Project-Scope", tc.scopeHeader)
			}
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tc.status, resp.StatusCode, string(body))
			if tc.status == http.StatusOK {
				require.JSONEq(t, fmt.Sprintf(`{"scope_project_uid":%q,"issue":{"uid":%q,"project_uid":%q}}`, scope.UID, issue.UID, target.UID), string(body))
			}
		})
	}
}

func (a historicalUIReferenceHostAccess) Authorize(_ context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	if request.Operation.AllProjects || slices.Contains(request.Operation.ProjectIDs, a.deniedProjectID) {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostAccessDecision{}, nil
}

func getUIIssueReference(t *testing.T, server *httptest.Server, query url.Values) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/ui/issue-reference?"+query.Encode(), nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(body)
}

func TestUIIssueReferenceHistoricalCurrentProject(t *testing.T) {
	for _, state := range []string{"active", "archived", "host-active", "host-denied", "same-project-denied", "scope-denied"} {
		t.Run(state, func(t *testing.T) {
			dbh := openTestDB(t)
			source, err := dbh.db.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			target, err := dbh.db.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			issue, _, err := dbh.db.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: source.ID, Title: "Moved browser target", Author: "worker",
			})
			require.NoError(t, err)
			if state != "same-project-denied" {
				_, err = dbh.db.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
					IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
					IfMatchRev: issue.Revision, Actor: "worker",
				})
				require.NoError(t, err)
			}
			if state == "archived" {
				_, _, err = dbh.db.RemoveProject(t.Context(), db.RemoveProjectParams{
					ProjectID: target.ID, Actor: "worker", Force: true,
				})
				require.NoError(t, err)
			}
			cfg := daemon.ServerConfig{DB: dbh.db, StartedAt: dbh.now}
			host := state == "host-active" || state == "host-denied" || state == "same-project-denied"
			if host {
				access := historicalUIReferenceHostAccess{}
				switch state {
				case "host-denied":
					access.deniedProjectID = target.ID
				case "same-project-denied":
					access.deniedProjectID = source.ID
				}
				cfg.HostAccess = access
			}
			server := daemon.NewServer(cfg)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if host {
					ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{
						Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "worker",
					})
					r = r.WithContext(ctx)
				}
				server.Handler().ServeHTTP(w, r)
			}))
			t.Cleanup(ts.Close)
			for _, ref := range []string{issue.ShortID, source.Name + "#" + issue.ShortID, issue.UID} {
				t.Run(ref, func(t *testing.T) {
					query := url.Values{"project_id": {fmt.Sprint(source.ID)}, "ref": {ref}}
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/ui/issue-reference?"+query.Encode(), nil)
					require.NoError(t, err)
					if state == "scope-denied" {
						req.Header.Set("X-Kata-Project-Scope", fmt.Sprintf(`["%s"]`, source.UID))
					}
					response, err := ts.Client().Do(req)
					require.NoError(t, err)
					defer func() { _ = response.Body.Close() }()
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					if state != "active" && state != "host-active" {
						require.Equal(t, http.StatusNotFound, response.StatusCode, string(body))
						require.NotContains(t, string(body), issue.UID)
						require.NotContains(t, string(body), target.UID)
						return
					}
					require.Equal(t, http.StatusOK, response.StatusCode, string(body))
					require.JSONEq(t, fmt.Sprintf(`{"issue":{"uid":"%s","project_uid":"%s"}}`, issue.UID, target.UID), string(body))
				})
			}
		})
	}
}

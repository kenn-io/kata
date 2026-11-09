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

func (a historicalUIReferenceHostAccess) Authorize(_ context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	if request.Operation.AllProjects || slices.Contains(request.Operation.ProjectIDs, a.deniedProjectID) {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostAccessDecision{}, nil
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

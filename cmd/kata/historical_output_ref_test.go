package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func historicalOutputFixture(t *testing.T, options ...testenv.Option) (*testenv.Env, string, db.Project, db.Issue, string) {
	t.Helper()
	return historicalOutputFixtureWithTarget(t, "target-project", options...)
}

func historicalOutputFixtureWithTarget(t *testing.T, targetName string, options ...testenv.Option) (*testenv.Env, string, db.Project, db.Issue, string) {
	t.Helper()
	env := testenv.New(t, options...)
	dir, sourceID := initLocalBoundWorkspace(t, env, "source-project")
	target, err := env.DB.CreateProject(t.Context(), targetName)
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sourceID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "Moved task", Author: "worker"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "Destination collision", Author: "worker"})
	require.NoError(t, err)
	moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: sourceID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	unrelated, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sourceID, UID: "11HZNQ7VFPK1XGD8R5MABCD4EX", Title: "Unrelated source task", Author: "worker"})
	require.NoError(t, err)
	require.Equal(t, moved.Issue.ShortID, unrelated.ShortID, "a bare current short ID addresses another issue in this workspace")
	return env, dir, target, moved.Issue, issue.ShortID
}

func TestHistoricalIssueOutputRefRoundTripsToMovedIssue(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		for _, operation := range []string{"show", "edit", "label add", "label rm", "restore", "assign", "unassign", "claim", "comment", "comment edit"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				if mode == "human" && strings.HasPrefix(operation, "comment") {
					t.Skip("human comment acknowledgements contain no issue reference")
				}
				env, dir, target, moved, oldRef := historicalOutputFixture(t)
				args := []string{"show", oldRef}
				switch operation {
				case "edit":
					args = []string{"edit", oldRef, "--title", "Continued work"}
				case "label add":
					args = []string{"label", "add", "source-project#" + oldRef, "example-label"}
				case "label rm":
					_, err := env.DB.AddLabel(t.Context(), moved.ID, "example-label", "worker")
					require.NoError(t, err)
					args = []string{"label", "rm", oldRef, "example-label"}
				case "assign":
					args = []string{"assign", oldRef, "worker"}
				case "unassign":
					runCLI(t, env, dir, "assign", oldRef, "worker")
					args = []string{"unassign", oldRef}
				case "claim":
					args = []string{"claim", oldRef}
				case "comment":
					args = []string{"comment", oldRef, "--body", "Continued discussion"}
				case "comment edit":
					comment, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: moved.ID, Body: "Earlier discussion", Author: "worker"})
					require.NoError(t, err)
					args = []string{"comment", "edit", oldRef, comment.UID, "--body", "Updated discussion"}
				case "restore":
					_, _, _, err := env.DB.SoftDeleteIssue(t.Context(), moved.ID, "worker")
					require.NoError(t, err)
					args = []string{"restore", oldRef}
				}
				args = append(args, "--format", mode)
				output := runCLI(t, env, dir, args...)
				qualified := target.Name + "#" + moved.ShortID
				if mode == "json" {
					var response struct {
						Issue struct {
							ProjectID int64  `json:"project_id"`
							UID       string `json:"uid"`
							ShortID   string `json:"short_id"`
						} `json:"issue"`
					}
					require.NoError(t, json.Unmarshal([]byte(output), &response))
					require.Equal(t, moved.UID, response.Issue.UID)
					require.Equal(t, moved.ShortID, response.Issue.ShortID)
					require.Equal(t, target.ID, response.Issue.ProjectID)
					return
				}
				first := strings.Fields(strings.SplitN(output, "\n", 2)[0])
				ref := first[0]
				if mode == "agent" {
					ref = first[2]
					require.Contains(t, output, "Issue: "+qualified+" ")
				}
				require.Equal(t, qualified, ref, output)
				var shown struct {
					Issue struct {
						UID string `json:"uid"`
					} `json:"issue"`
				}
				require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "show", ref, "--json")), &shown))
				require.Equal(t, moved.UID, shown.Issue.UID, "copying the displayed ref must retain issue identity")
			})
		}
	}
}

func TestHistoricalOutputUsesResolvedProjectContext(t *testing.T) {
	for _, mode := range []string{"human", "agent"} {
		for _, historical := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/current", true: "/historical"}[historical], func(t *testing.T) {
				env, dir, target, moved, oldRef := historicalOutputFixture(t)
				ref, expected := moved.ShortID, moved.ShortID
				if historical {
					ref, expected = "source-project#"+oldRef, target.Name+"#"+moved.ShortID
				}
				output := runCLI(t, env, dir, "--project", target.Name, "show", ref, "--format", mode)
				first := strings.Fields(strings.SplitN(output, "\n", 2)[0])
				if mode == "agent" {
					require.Equal(t, expected, first[2])
				} else {
					require.Equal(t, expected, first[0])
				}
			})
		}
	}
}

func TestHistoricalOutputLookupFailureUsesStableUID(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		for _, operation := range []string{"show", "label no-op", "assign no-op", "claim no-op"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				env, dir, target, moved, oldRef := historicalOutputFixture(t)
				upstream, err := url.Parse(env.URL)
				require.NoError(t, err)
				forward := &httputil.ReverseProxy{Rewrite: func(request *httputil.ProxyRequest) { request.SetURL(upstream) }}
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == fmt.Sprintf("/api/v1/projects/%d", target.ID) {
						http.Error(w, "project display unavailable", http.StatusServiceUnavailable)
						return
					}
					forward.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				env.URL = proxy.URL
				args := []string{"show", oldRef}
				if operation == "label no-op" {
					_, err := env.DB.AddLabel(t.Context(), moved.ID, "example-label", "worker")
					require.NoError(t, err)
					args = []string{"label", "add", "source-project#" + oldRef, "example-label"}
				}
				if operation == "assign no-op" || operation == "claim no-op" {
					runCLIAs(t, env, dir, "worker", "assign", oldRef, "worker")
					args = []string{"assign", oldRef, "worker"}
					if operation == "claim no-op" {
						args = []string{"claim", oldRef}
					}
				}
				output := runCLIAs(t, env, dir, "worker", append(args, "--format", mode)...)
				if mode == "json" {
					var response struct {
						Issue db.Issue `json:"issue"`
					}
					require.NoError(t, json.Unmarshal([]byte(output), &response))
					require.Equal(t, moved.UID, response.Issue.UID)
					require.Equal(t, moved.ShortID, response.Issue.ShortID)
					return
				}
				first := strings.Fields(strings.SplitN(output, "\n", 2)[0])
				ref := first[0]
				if mode == "agent" {
					ref = first[2]
				}
				require.Equal(t, moved.UID, ref, "failed display lookup must not print the unrelated source issue's short ID")
				var response struct {
					Issue db.Issue `json:"issue"`
				}
				require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "show", ref, "--json")), &response))
				require.Equal(t, moved.UID, response.Issue.UID)
			})
		}
	}
}

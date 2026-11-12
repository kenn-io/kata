package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMoveBatchCLI_EncodedPages(t *testing.T) {
	env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-encoded-pages"))
	target, err := env.DB.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	name := strings.Repeat("a", 1100)
	source, err := env.DB.CreateProject(t.Context(), name)
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "long name", Author: "tester"})
	require.NoError(t, err)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	pages, failSecond := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/issues/actions/move" {
			pages++
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.LessOrEqual(t, len(body), 1<<20)
			require.Contains(t, string(body), `\u003c`)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			if failSecond && pages == 2 {
				http.Error(w, "unavailable", http.StatusInternalServerError)
				return
			}
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	invalid := strings.Repeat("<", 100000) + "#abcd"
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, "", "--workspace", dir, "move", invalid, name+"#"+issue.ShortID, invalid, target.Name, "--json")
	require.Equal(t, ExitConflict, exitCodeForErr(err, true))
	require.Equal(t, 2, pages)
	var reply api.MoveIssuesResponse
	require.NoError(t, json.Unmarshal([]byte(out), &reply.Body))
	require.Len(t, reply.Body.Results, 3)
	for i, want := range []string{"refused", "moved", "refused"} {
		require.Equal(t, i, reply.Body.Results[i].Index)
		require.Equal(t, want, reply.Body.Results[i].Result)
	}
	got, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	failSecond, pages = true, 0
	out, _, err = executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, "", "--workspace", dir, "move", invalid, name+"#"+issue.ShortID, invalid, target.Name, "--json")
	require.Error(t, err)
	require.Equal(t, 2, pages)
	require.NoError(t, json.Unmarshal([]byte(out), &reply.Body))
	for i, want := range []string{"refused", "unchanged", "unknown"} {
		require.Equal(t, i, reply.Body.Results[i].Index)
		require.Equal(t, want, reply.Body.Results[i].Result)
	}
}

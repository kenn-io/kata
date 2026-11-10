package main

import (
	"encoding/json/v2"
	"fmt"
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

func TestParseRefsFrom(t *testing.T) {
	for _, input := range []string{"abc4\n", "abc4\tdst\n", "# note\n\n abc4 \r\n", "\ufeffabc4\n"} {
		items, err := parseRefsFrom(strings.NewReader(input), "dst")
		require.NoError(t, err)
		require.Len(t, items, 1)
		require.Equal(t, "abc4", items[0].Ref)
		require.Equal(t, "dst", items[0].TargetProject)
	}
	_, err := parseRefsFrom(strings.NewReader("abc4\n"), "")
	require.ErrorContains(t, err, "line 1")
}
func TestMoveBatchCLI_PositionalRerun(t *testing.T) {
	env, dir, src, dst, first := setupMoveCLIProjects(t)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	args := []string{"--project", src.Name, "move", first.ShortID, second.ShortID, dst.Name, "--agent", "--comment", "routed"}
	out := runCLI(t, env, dir, args...)
	require.Contains(t, out, "OK move-batch requested=2 moved=2 unchanged=0 refused=0 dry_run=false")
	again := runCLI(t, env, dir, args...)
	require.Contains(t, again, "moved=0 unchanged=2 refused=0")
	for _, i := range []db.Issue{first, second} {
		comments, err := env.DB.CommentsByIssue(t.Context(), i.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
	}
}
func TestMoveBatchCLI_SelectorLabel(t *testing.T) {
	env, dir, src, dst, first := setupMoveCLIProjects(t)
	_, err := env.DB.AddLabel(t.Context(), first.ID, "routing", "tester")
	require.NoError(t, err)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "stay", Author: "tester"})
	require.NoError(t, err)
	out := runCLI(t, env, dir, "move", "--from-project", src.Name, "--label", "routing", dst.Name, "--agent")
	require.Contains(t, out, "requested=1 moved=1")
	got, err := env.DB.IssueByID(t.Context(), second.ID)
	require.NoError(t, err)
	require.Equal(t, src.ID, got.ProjectID)
}

// Contract: line-specific targets, global UIDs and qualified refs work together;
// preview returns machine-readable rows and never creates a routing comment.
func TestMoveBatchCLI_StdinMixedTargetsDryRun(t *testing.T) {
	env, dir, src, dst, first := setupMoveCLIProjects(t)
	other, err := env.DB.CreateProject(t.Context(), "second-target")
	require.NoError(t, err)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	input := "\ufeff# routing\r\n" + src.Name + "#" + first.ShortID + "\t" + dst.Name + "\r\n" + second.UID + "\t" + other.Name + "\n"
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), env.URL), t, input,
		"--workspace", dir, "move", "--refs-from", "-", "--dry", "--json", "--comment", "routed")
	require.NoError(t, err)
	var reply struct {
		DryRun  bool                   `json:"dry_run"`
		Summary api.MoveIssuesSummary  `json:"summary"`
		Results []api.MoveIssuesResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.True(t, reply.DryRun)
	require.Equal(t, 2, reply.Summary.WouldMove)
	require.Len(t, reply.Results, 2)
	for _, issue := range []db.Issue{first, second} {
		got, err := env.DB.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Equal(t, src.ID, got.ProjectID)
		comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Empty(t, comments)
	}
}

// Contract: selectors exhaust pages before writing and carry the captured
// revision, so a concurrent edit refuses only that issue.
func TestMoveBatchCLI_SelectorPagingRevisionFence(t *testing.T) {
	// Forwarded requests need explicit bearer authority, just as a real proxy does.
	env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-move-proxy-token"))
	src, err := env.DB.CreateProject(t.Context(), "selector-source")
	require.NoError(t, err)
	dst, err := env.DB.CreateProject(t.Context(), "selector-target")
	require.NoError(t, err)
	first, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "first", Author: "tester"})
	require.NoError(t, err)
	for range 249 {
		_, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "route", Author: "tester"})
		require.NoError(t, err)
	}
	_, _, _, err = env.DB.CloseIssue(t.Context(), first.ID, "done", "tester", "Verified fixture completion", []db.Evidence{{Type: "test", Command: "synthetic fixture"}})
	require.NoError(t, err)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode >= 400 {
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				return err
			}
			t.Logf("upstream %s %s returned %d: %s", response.Request.Method, response.Request.URL, response.StatusCode, raw)
			response.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		return nil
	}
	changed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/issues/actions/move" && !changed {
			changed = true
			updated, _, changed, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: first.ID, Owner: new("another-worker"), Actor: "tester"})
			require.NoError(t, err)
			require.True(t, changed)
			require.Greater(t, updated.Revision, first.Revision)
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	out, _, err := runCLIAgainst(t, server.URL, dir, "move", "--from-project", src.Name, "--status", "all", dst.Name, "--json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "some issues were refused")
	var reply struct {
		Results []api.MoveIssuesResult `json:"results"`
		Summary api.MoveIssuesSummary  `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 250)
	require.Equal(t, 249, reply.Summary.Moved)
	require.Equal(t, 1, reply.Summary.Refused)
	require.Equal(t, first.UID, reply.Results[0].UID)
	require.Equal(t, "revision_conflict", reply.Results[0].Code)
	var cliErr *cliError
	require.ErrorAs(t, err, &cliErr)
	require.Equal(t, 5, cliErr.ExitCode)
}

// Contract: each body respects both limits and output indexes remain global.
// A lost later response preserves rows already acknowledged and exits 7.
func TestMoveBatchCLI_RequestSplittingAndTransportFailure(t *testing.T) {
	for _, mode := range []string{"count", "bytes", "transport"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, _, dst, first := setupMoveCLIProjects(t)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			calls := 0
			lengths := []int{}
			sizes := []int{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/issues/actions/move" {
					r.Host = origin.Host
					proxy.ServeHTTP(w, r)
					return
				}
				calls++
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				sizes = append(sizes, len(raw))
				var request api.MoveIssuesRequest
				require.NoError(t, json.Unmarshal(raw, &request.Body))
				lengths = append(lengths, len(request.Body.Items))
				if mode == "transport" && calls == 2 {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
					return
				}
				end := len(request.Body.Items)
				if mode == "transport" {
					end = 50
				}
				results := []api.MoveIssuesResult{}
				for j := request.Body.StartIndex; j < end; j++ {
					results = append(results, api.MoveIssuesResult{Index: j, Ref: request.Body.Items[j].Ref, Result: "moved"})
				}
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.MarshalWrite(w, map[string]any{"results": results, "next_index": end}))
			}))
			t.Cleanup(server.Close)
			count := 5001
			comment := ""
			if mode == "bytes" {
				comment = strings.Repeat("x", 700000)
			}
			if mode == "transport" {
				count = 120
			}
			input := strings.Repeat(first.UID+"\n", count)
			args := []string{"--workspace", dir, "move", dst.Name, "--refs-from", "-", "--json"}
			if comment != "" {
				args = append(args, "--comment", comment)
			}
			out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, input, args...)
			var reply struct {
				Results []api.MoveIssuesResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			if mode == "transport" {
				var ce *cliError
				require.ErrorAs(t, err, &ce)
				require.Equal(t, 7, ce.ExitCode)
				require.Len(t, reply.Results, 50)
			} else {
				require.NoError(t, err)
				require.Len(t, reply.Results, count)
			}
			require.Greater(t, calls, 1)
			for _, n := range lengths {
				require.LessOrEqual(t, n, 5000)
			}
			for _, n := range sizes {
				require.LessOrEqual(t, n, 1<<20)
			}
			for j, row := range reply.Results {
				require.Equal(t, j, row.Index, fmt.Sprint(mode, j))
			}
		})
	}
}
func TestMoveBatchCLI_OldDaemonGate(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.44.0"}`))
			return
		}
		calls++
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	_, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), "move", "abc4", "def5", "target")
	require.ErrorContains(t, err, "requires daemon API 0.45.0")
	require.Zero(t, calls)
}

package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMoveBatchCLI_ContinuationAfterCompletedDuplicate(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t, testenv.WithAuthToken("synthetic-continuation-token"), func(cfg *daemon.ServerConfig) {
		cfg.MoveBatchBudget = time.Nanosecond //nolint:kennlint // deadlinetest: Force a yield after one completed database chunk.
	})
	dir, _ := initLocalBoundWorkspace(t, env, "kata")
	source, err := env.DB.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "selected issue", Author: "tester"})
	require.NoError(t, err)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	var continuation api.MoveIssuesRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/issues/actions/move" {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			var body api.MoveIssuesRequest
			if err := json.Unmarshal(raw, &body.Body); err != nil {
				t.Error(err)
				return
			}
			if body.Body.StartIndex > 0 {
				continuation = body
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, "", "--workspace", dir, "move", issue.UID, target.Name, "--from-project", source.Name, "--status", "all", "--json")
	require.NoError(t, err)
	var reply struct {
		Results []api.MoveIssuesResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 2)
	require.Equal(t, "moved", reply.Results[0].Result)
	require.Equal(t, "unchanged", reply.Results[1].Result)
	require.Equal(t, 1, continuation.Body.StartIndex)
	require.Len(t, continuation.Body.Completed, 1)
	require.Equal(t, reply.Results[0].EventID, continuation.Body.Completed[0].EventID)
	require.Equal(t, reply.Results[0].Revision, continuation.Body.Completed[0].Revision)
	require.NotEmpty(t, reply.Results[0].Proof)
	require.Equal(t, reply.Results[0].Proof, continuation.Body.Completed[0].Proof)
	require.Equal(t, issue.Revision, continuation.Body.Items[1].ExpectedRevision)
}

func TestParseRefsFrom(t *testing.T) {
	for _, input := range []string{"abc4\n", "abc4\tdst\n", "# note\n\n abc4 \r\n", "\ufeffabc4\n"} {
		items, err := parseRefsFrom(strings.NewReader(input), "dst")
		require.NoError(t, err)
		require.Len(t, items, 1)
		require.Equal(t, "abc4", items[0].Ref)
		require.Equal(t, "dst", items[0].TargetProject)
	}
	items, err := parseRefsFrom(strings.NewReader("abc4\n"), "")
	require.NoError(t, err)
	require.Contains(t, items[0].ParseError, "line 1")
	for _, row := range []string{"abc4\t\n", "abc4\ttarget\t\n", "\tabc4\n"} {
		items, err := parseRefsFrom(strings.NewReader("# routing\n"+row), "default-target")
		require.NoError(t, err)
		require.Contains(t, items[0].ParseError, "line 2")
	}
	reader := strings.NewReader(strings.Repeat("abcd\n", 5001) + strings.Repeat("x", 2<<20))
	_, err = parseRefsFrom(reader, "dst")
	require.ErrorContains(t, err, "5,000")
	require.Greater(t, reader.Len(), 1<<20)
	reader = strings.NewReader(strings.Repeat(strings.Repeat("a", 600000)+"\n", 4))
	_, err = parseRefsFrom(reader, "dst")
	require.ErrorContains(t, err, "1 MiB")
	require.Greater(t, reader.Len(), 1<<20)
}

func TestMoveBatchCLI_InvalidRowsDoNotBlockValid(t *testing.T) {
	for _, mode := range []outputMode{outputHuman, outputJSON, outputAgent} {
		for _, allInvalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s all-invalid=%t", mode, allInvalid), func(t *testing.T) {
				env, dir, _, dst, issue := setupMoveCLIProjects(t)
				input := "\tdst\n"
				if !allInvalid {
					input += issue.UID + "\t" + dst.Name + "\n"
				}
				args := []string{"--workspace", dir, "move", "--refs-from", "-"}
				switch mode {
				case outputJSON:
					args = append(args, "--json")
				case outputAgent:
					args = append(args, "--agent")
				}
				out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), env.URL), t, input, args...)
				require.Error(t, err)
				require.Equal(t, ExitConflict, exitCodeForErr(err, true))
				var stderr bytes.Buffer
				emitErrorForMode(&stderr, err, mode, true)
				if mode == outputHuman {
					require.Contains(t, out, "1 refused")
					require.Empty(t, stderr.String())
				} else {
					require.Contains(t, stderr.String(), "some issues were refused")
				}
				if mode == outputJSON && !allInvalid {
					var reply struct {
						Results []api.MoveIssuesResult `json:"results"`
					}
					require.NoError(t, json.Unmarshal([]byte(out), &reply))
					require.Len(t, reply.Results, 2)
					require.Equal(t, 0, reply.Results[0].Index)
					require.Equal(t, "validation", reply.Results[0].Code)
					require.Equal(t, "refused", reply.Results[0].Result)
					require.Equal(t, 1, reply.Results[1].Index)
					require.Equal(t, "moved", reply.Results[1].Result)
					got, err := env.DB.IssueByID(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Equal(t, dst.ID, got.ProjectID)
				}
			})
		}
	}

	for _, mode := range []string{"missing target", "invalid ref", "no workspace", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, src, dst, issue := setupMoveCLIProjects(t)
			var input string
			args := []string{"--workspace", dir, "move", "--refs-from", "-", "--json"}
			switch mode {
			case "missing target":
				input = issue.UID + "\tmissing-project\n" + issue.UID + "\t" + dst.Name + "\n"
			case "invalid ref":
				input = ""
				args = []string{"--workspace", dir, "--project", src.Name, "move", "invalid!", issue.ShortID, dst.Name, "--json"}
			case "oversized":
				input = strings.Repeat("x", 1<<20) + "\n" + issue.UID + "\t" + dst.Name + "\n"
			case "no workspace":
				args[1] = t.TempDir()
				input = "abcd\t" + dst.Name + "\n" + issue.UID + "\t" + dst.Name + "\n"
			}
			out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), env.URL), t, input, args...)
			require.Error(t, err)
			var reply struct {
				Results []api.MoveIssuesResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			require.Len(t, reply.Results, 2)
			require.Equal(t, 0, reply.Results[0].Index)
			if mode == "oversized" {
				require.Equal(t, "validation", reply.Results[0].Code)
			}
			require.Equal(t, "refused", reply.Results[0].Result)
			require.Equal(t, 1, reply.Results[1].Index)
			require.Equal(t, "moved", reply.Results[1].Result)
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, dst.ID, got.ProjectID)
		})
	}
}

func TestMoveBatchCLI_TargetFailureIsPerItem(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t, testenv.WithAuthToken("synthetic-target-token"))
	dir, _ := initLocalBoundWorkspace(t, env, "kata")
	src, err := env.DB.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	dst, err := env.DB.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	first, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "first", Author: "tester"})
	require.NoError(t, err)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "independent", Author: "tester"})
	require.NoError(t, err)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	failed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" && !failed {
			failed = true
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	input := first.UID + "\t" + dst.Name + "\n" + second.UID + "\t" + dst.Name + "\n"
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, input, "--workspace", dir, "move", "--refs-from", "-", "--json")
	require.Error(t, err)
	var reply struct {
		Results []api.MoveIssuesResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 2, "command error: %v", err)
	require.Equal(t, "refused", reply.Results[0].Result)
	require.Equal(t, "moved", reply.Results[1].Result)
	got, err := env.DB.IssueByID(t.Context(), second.ID)
	require.NoError(t, err)
	require.Equal(t, dst.ID, got.ProjectID)
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
// preview returns machine-readable rows.
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
	require.Equal(t, dst.Name, reply.Results[0].ToProjectName)
	require.Equal(t, other.Name, reply.Results[1].ToProjectName)
	for _, issue := range []db.Issue{first, second} {
		got, err := env.DB.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Equal(t, src.ID, got.ProjectID)
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

// Contract: oversized invocations refuse before any write, including previews.
// A lost later response preserves rows already acknowledged and exits 7.
func TestMoveBatchCLI_LimitsAndTransportFailure(t *testing.T) {
	for _, mode := range []string{"count", "bytes", "bytes-escaped", "transport"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, _, dst, first := setupMoveCLIProjects(t)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/issues/actions/move" {
					r.Host = origin.Host
					proxy.ServeHTTP(w, r)
					return
				}
				calls++
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				var request api.MoveIssuesRequest
				require.NoError(t, json.Unmarshal(raw, &request.Body))
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
			if strings.HasPrefix(mode, "bytes") {
				count = 4000
				comment = strings.Repeat("x", 800000)
			}
			if strings.HasPrefix(mode, "bytes-escaped") {
				count = 1
				comment = strings.Repeat("<", 200000)
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
			if mode != "transport" {
				var ce *cliError
				require.ErrorAs(t, err, &ce)
				require.Equal(t, ExitUsage, ce.ExitCode)
				limit := "5,000"
				if strings.HasPrefix(mode, "bytes") {
					limit = "1 MiB"
				}
				require.Contains(t, ce.Message, limit)
				require.Contains(t, ce.Message, "separate runs")
				require.Zero(t, calls)
				got, e := env.DB.IssueByID(t.Context(), first.ID)
				require.NoError(t, e)
				require.Equal(t, first.ProjectID, got.ProjectID)
				return
			}
			var reply struct {
				Results []api.MoveIssuesResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			var ce *cliError
			require.ErrorAs(t, err, &ce)
			require.Equal(t, 7, ce.ExitCode)
			require.Len(t, reply.Results, 50)
			require.Greater(t, calls, 1)
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

func TestMoveBatchCLI_SelectorFlagsRequireSource(t *testing.T) {
	for _, flag := range [][]string{{"--status", "all"}, {"--query", "route"}, {"--label", "absent"}, {"--no-label", "skip"}, {"--owner", "worker"}, {"--unowned"}, {"--meta", "key=value"}} {
		t.Run(strings.Join(flag, "_"), func(t *testing.T) {
			args := append([]string{"move", "abc4", "target"}, flag...)
			_, _, err := executeRootCapture(t, t.Context(), args...)
			var ce *cliError
			require.ErrorAs(t, err, &ce)
			require.Equal(t, ExitUsage, ce.ExitCode)
			require.Contains(t, ce.Message, "--from-project")
		})
	}
}

// A healthy mutation may exceed the standard five-second read budget, while
// cancellation of its invocation must still stop it.
func TestMoveBatchCLI_LongMutationAndCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			env, dir, _, dst, first := setupMoveCLIProjects(t)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			ctx, cancel := context.WithCancel(contextWithBaseURL(t.Context(), env.URL))
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/issues/actions/move" {
					r.Host = origin.Host
					proxy.ServeHTTP(w, r)
					return
				}
				if cancelRequest {
					cancel()
					return
				}
				//nolint:kennlint // sleeptest: Exercise a healthy mutation beyond the normal HTTP client deadline.
				timer := time.NewTimer(5200 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.MarshalWrite(w, map[string]any{"results": []api.MoveIssuesResult{{Index: 0, Ref: first.UID, Result: "moved"}}, "next_index": 1})
			}))
			defer server.Close()
			_, _, err = executeRootCaptureWithInput(contextWithBaseURL(ctx, server.URL), t, first.UID+"\n", "--workspace", dir, "move", dst.Name, "--refs-from", "-", "--json")
			if cancelRequest {
				require.ErrorContains(t, err, "request canceled")
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMoveBatchCLI_PartialErrorResults(t *testing.T) {
	env, dir, _, dst, issue := setupMoveCLIProjects(t)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/issues/actions/move" {
			r.Host = origin.Host
			proxy.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		require.NoError(t, json.MarshalWrite(w, map[string]any{"status": 500, "error": map[string]any{"code": "internal", "message": "database unavailable", "data": map[string]any{"results": []api.MoveIssuesResult{{Index: 0, Ref: issue.UID, Result: "moved"}, {Index: 50, Ref: issue.UID, Result: "refused", Code: "validation"}}, "next_index": 1}}}))
	}))
	t.Cleanup(server.Close)
	input := issue.UID + "\tmissing-project\n" + strings.Repeat(issue.UID+"\t"+dst.Name+"\n", 51)
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, input, "--workspace", dir, "move", "--refs-from", "-", "--json")
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, 500, ce.HTTPStatus)
	var reply struct {
		Results []api.MoveIssuesResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 3)
	require.Equal(t, 0, reply.Results[0].Index)
	require.Equal(t, "refused", reply.Results[0].Result)
	require.Equal(t, 1, reply.Results[1].Index)
	require.Equal(t, "moved", reply.Results[1].Result)
	require.Equal(t, 51, reply.Results[2].Index)
	require.Equal(t, "refused", reply.Results[2].Result)
}

func TestMoveBatchCLI_ReportsEarlierResultsOnInvalidReply(t *testing.T) {
	for _, mode := range []string{"decode", "continuation", "expired_proof"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, _, target, issue := setupMoveCLIProjects(t)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/issues/actions/move" {
					r.Host = origin.Host
					proxy.ServeHTTP(w, r)
					return
				}
				calls++
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					require.NoError(t, json.MarshalWrite(w, map[string]any{"results": []api.MoveIssuesResult{{Index: 0, Ref: issue.UID, Result: "moved"}}, "next_index": 1}))
				} else if mode == "decode" {
					_, err := io.WriteString(w, "{")
					require.NoError(t, err)
				} else if mode == "expired_proof" {
					w.WriteHeader(http.StatusBadRequest)
					require.NoError(t, json.MarshalWrite(w, map[string]any{"status": 400, "error": map[string]any{"code": "invalid_continuation", "message": "continuation proof is invalid or expired", "data": map[string]any{"results": []api.MoveIssuesResult{}, "next_index": 1}}}))
				} else {
					_, err := io.WriteString(w, `{"results":[],"next_index":1}`)
					require.NoError(t, err)
				}
			}))
			t.Cleanup(server.Close)
			input := issue.UID + "\n" + issue.UID + "\n"
			out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, input, "--workspace", dir, "move", target.Name, "--refs-from", "-", "--json")
			require.Error(t, err)
			var reply struct {
				Results []api.MoveIssuesResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			require.Len(t, reply.Results, 1)
			require.Equal(t, "moved", reply.Results[0].Result)
			require.Equal(t, 0, reply.Results[0].Index)
		})
	}
}

func TestMoveBatchCLI_HumanShowsEveryResult(t *testing.T) {
	env, dir, src, dst, first := setupMoveCLIProjects(t)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	args := []string{"--project", src.Name, "move", first.ShortID, second.ShortID, dst.Name}
	preview := runCLI(t, env, dir, append(append([]string{}, args...), "--dry")...)
	require.Contains(t, preview, "ref="+first.ShortID)
	require.Contains(t, preview, "ref="+second.ShortID)
	out := runCLI(t, env, dir, args...)
	require.Contains(t, out, "ref="+first.ShortID)
	require.Contains(t, out, "ref="+second.ShortID)
	out = runCLI(t, env, dir, args...)
	require.Contains(t, out, "result=unchanged")
	require.Contains(t, out, "ref="+first.ShortID)
	require.Contains(t, out, "ref="+second.ShortID)
}

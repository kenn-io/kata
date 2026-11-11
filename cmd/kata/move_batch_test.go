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
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMoveBatchCLI_Pages(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(failSecond), func(t *testing.T) {
			env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-page-token"))
			source, err := env.DB.CreateProject(t.Context(), "source-project")
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "target-project")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "issue", Author: "tester"})
			require.NoError(t, err)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/issues/actions/move" {
					calls++
					if failSecond && calls == 2 {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return
					}
				}
				r.Host = origin.Host
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, strings.Repeat(issue.UID+"\n", 51), "--workspace", dir, "move", target.Name, "--refs-from", "-", "--json")
			if failSecond {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 2, calls)
			var reply struct {
				Results []api.MoveIssuesResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			require.Len(t, reply.Results, 51)
			if failSecond {
				require.Equal(t, "unknown", reply.Results[50].Result)
			}
			require.Equal(t, "moved", reply.Results[0].Result)
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, got.ProjectID)
		})
	}
}

func TestParseRefsFrom(t *testing.T) {
	for _, input := range []string{"abc4\n", "abc4\tdst\n", "# note\n\n abc4 \r\n", "\ufeffabc4\n"} {
		items, err := parseRefsFrom(strings.NewReader(input), "dst", 5001)
		require.NoError(t, err)
		require.Len(t, items, 1)
		require.Equal(t, "abc4", items[0].Ref)
		require.Equal(t, "dst", items[0].TargetProject)
	}
	items, err := parseRefsFrom(strings.NewReader("abc4\n"), "", 5001)
	require.NoError(t, err)
	require.Contains(t, items[0].ParseError, "line 1")
	for _, row := range []string{"abc4\t\n", "abc4\ttarget\t\n", "\tabc4\n"} {
		items, err := parseRefsFrom(strings.NewReader("# routing\n"+row), "default-target", 5001)
		require.NoError(t, err)
		require.Contains(t, items[0].ParseError, "line 2")
	}
	reader := strings.NewReader(strings.Repeat("abcd\n", 5001) + strings.Repeat("x", 2<<20))
	items, err = parseRefsFrom(reader, "dst", 5001)
	require.NoError(t, err)
	require.Len(t, items, 5001)
	require.Greater(t, reader.Len(), 1<<20)
	reader = strings.NewReader(strings.Repeat(strings.Repeat("a", 600000)+"\n", 4))
	_, err = parseRefsFrom(reader, "dst", 5001)
	require.ErrorContains(t, err, "1 MiB")
	require.Greater(t, reader.Len(), 1<<20)
	items, err = parseRefsFrom(strings.NewReader(strings.Repeat("x", 1<<20)+"\nabc4\n"), "dst", 5001)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, "line 1: exceeds 1 MiB", items[0].ParseError)
	require.Equal(t, moveBatchItem{Ref: "abc4", TargetProject: "dst"}, items[1])
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
				}
				if !allInvalid {
					got, err := env.DB.IssueByID(t.Context(), issue.ID)
					require.NoError(t, err)
					require.Equal(t, dst.ID, got.ProjectID)
				}
			})
		}
	}

	for _, mode := range []string{"missing target", "invalid ref", "no workspace"} {
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
			if mode == "missing target" {
				require.Equal(t, `project selector "missing-project" did not match any project`, reply.Results[0].Message)
			}
			require.Equal(t, "refused", reply.Results[0].Result)
			require.Equal(t, 1, reply.Results[1].Index)
			require.Equal(t, "moved", reply.Results[1].Result)
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, dst.ID, got.ProjectID)
		})
	}
	t.Run("temporary target failure", func(t *testing.T) {
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

	})
}

func TestMoveBatchCLI_SelectionRestrictions(t *testing.T) {
	for _, tc := range []struct{ rule, mode string }{
		{"dry", "refs"}, {"dry", "file"}, {"dry", "selector"},
		{"subtree", "refs"}, {"subtree", "file"}, {"subtree", "selector"}, {"subtree", "single"},
	} {
		t.Run(tc.rule+"/"+tc.mode, func(t *testing.T) {
			env, dir, source, target, first := setupMoveCLIProjects(t)
			second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "second", Author: "tester"})
			require.NoError(t, err)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				r.Host = origin.Host
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			args := []string{"--workspace", dir, "move", "--" + tc.rule}
			input := ""
			switch tc.mode {
			case "refs":
				args = append(args, first.UID, second.UID, target.Name)
			case "single":
				args = append(args, "--single", first.UID, target.Name)
			case "file":
				args = append(args, "--refs-from", "-", target.Name)
				input = first.UID + "\n" + second.UID + "\n"
			case "selector":
				args = append(args, "--from-project", source.Name, "--status", "all", target.Name)
			}
			out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, input, args...)
			message := "--subtree accepts one issue"
			if tc.rule == "dry" {
				message = "dry run accepts one item"
			} else if tc.mode == "single" {
				message = "--single and --subtree are mutually exclusive"
			}
			require.ErrorContains(t, err, message)
			require.Equal(t, ExitUsage, exitCodeForErr(err, true))
			require.Empty(t, out)
			if tc.rule == "dry" && tc.mode != "selector" {
				require.Zero(t, requests)
			}
			for _, issue := range []db.Issue{first, second} {
				got, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, source.ID, got.ProjectID)
			}
		})
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

// Contract: selectors exhaust pages before writing and carry the captured
// revision, so a concurrent edit refuses only that issue.
func TestMoveBatchCLI_SelectorPagingRevisionFence(t *testing.T) {
	// Forwarded requests need explicit bearer authority, just as a real proxy does.
	env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-move-proxy-token"))
	src, err := env.DB.CreateProject(t.Context(), "selector-source")
	require.NoError(t, err)
	dst, err := env.DB.CreateProject(t.Context(), "selector-target")
	require.NoError(t, err)
	first, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "first route", Author: "tester"})
	require.NoError(t, err)
	for range 249 {
		_, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "route", Author: "tester"})
		require.NoError(t, err)
	}
	_, _, _, err = env.DB.CloseIssue(t.Context(), first.ID, "done", "tester", "Verified fixture completion", []db.Evidence{{Type: "test", Command: "synthetic fixture"}})
	require.NoError(t, err)
	first, err = env.DB.IssueByID(t.Context(), first.ID)
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
			updated, _, edited, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: first.ID, Owner: new("another-worker"), Actor: "tester"})
			require.NoError(t, err)
			require.True(t, edited)
			require.Greater(t, updated.Revision, first.Revision)
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	out, _, err := runCLIAgainst(t, server.URL, dir, "move", "--from-project", src.Name, "--status", "all", "--query", "route", dst.Name, "--json")
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

func TestMoveBatchCLI_SelectorStopsAtLimit(t *testing.T) {
	for _, query := range []bool{false, true} {
		t.Run(fmt.Sprintf("query=%t", query), func(t *testing.T) {
			env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-selector-token"))
			source, err := env.DB.CreateProject(t.Context(), "selector-source")
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "selector-target")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "unique search term", Author: "tester"})
			require.NoError(t, err)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/issues", source.ID) && pages < 27 {
					pages++
					items := make([]map[string]any, 200)
					for i := range items {
						items[i] = map[string]any{"uid": fmt.Sprintf("%026d", (pages-1)*200+i+1), "revision": 1}
					}
					cursor := fmt.Sprint(pages)
					if pages == 27 {
						items = []map[string]any{{"uid": issue.UID, "revision": issue.Revision}}
						cursor = ""
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.MarshalWrite(w, map[string]any{"issues": items, "next_cursor": cursor})
					return
				}
				r.Host = origin.Host
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			args := []string{"move", "--from-project", source.Name, "--status", "all", "--dry"}
			if query {
				args = append(args, "--query", "unique")
			}
			args = append(args, target.Name, "--json")
			out, _, err := runCLIAgainst(t, server.URL, dir, args...)
			if query {
				require.NoError(t, err)
				require.Equal(t, 27, pages)
				require.Contains(t, out, issue.UID)
			} else {
				require.ErrorContains(t, err, "5,000")
				require.Equal(t, 26, pages)
			}
		})
	}
}

// Oversized invocations stop before any write.
func TestMoveBatchCLI_Limits(t *testing.T) {
	for _, mode := range []string{"count", "bytes", "bytes-escaped", "later-page"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, _, dst, first := setupMoveCLIProjects(t)
			count, comment := 5001, ""
			switch mode {
			case "bytes":
				count, comment = 1, strings.Repeat("x", 1<<20)
			case "bytes-escaped":
				count, comment = 1, strings.Repeat("<", 200000)
			case "later-page":
				count = 50
			}
			input := strings.Repeat(first.UID+"\n", count)
			if mode == "later-page" {
				input += strings.Repeat("<", 200000) + "\n"
			}
			args := []string{"--workspace", dir, "move", dst.Name, "--refs-from", "-", "--json"}
			if mode == "count" {
				args = []string{"--workspace", dir, "move", "--json"}
				for range count {
					args = append(args, first.UID)
				}
				args = append(args, dst.Name)
				input = ""
			}
			if comment != "" {
				args = append(args, "--comment", comment)
			}
			_, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), env.URL), t, input, args...)
			var ce *cliError
			require.ErrorAs(t, err, &ce)
			require.Equal(t, ExitUsage, ce.ExitCode)
			limit := "5,000"
			if mode != "count" {
				limit = "1 MiB"
			}
			require.Contains(t, ce.Message, limit)
			require.Contains(t, ce.Message, "separate runs")
			got, err := env.DB.IssueByID(t.Context(), first.ID)
			require.NoError(t, err)
			require.Equal(t, first.ProjectID, got.ProjectID)
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
				_ = json.MarshalWrite(w, map[string]any{"results": []api.MoveIssuesResult{{Index: 0, Ref: first.UID, Result: "moved"}}})
			}))
			defer server.Close()
			_, _, err = executeRootCaptureWithInput(contextWithBaseURL(ctx, server.URL), t, first.UID+"\n", "--workspace", dir, "move", dst.Name, "--refs-from", "-", "--json")
			if cancelRequest {
				require.ErrorContains(t, err, "request canceled")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// Version skew can produce outcomes the current daemon does not emit.
func TestMoveBatchCLI_RejectsUnknownResult(t *testing.T) {
	env, dir, _, target, issue := setupMoveCLIProjects(t)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/issues/actions/move" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"results":[{"index":0,"result":"unknown"}]}`)
			return
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, issue.UID+"\n", "--workspace", dir, "move", target.Name, "--refs-from", "-", "--json")
	require.ErrorContains(t, err, "invalid batch move result")
	require.Contains(t, out, `"result":"unknown"`)
}

func TestMoveBatchCLI_PreservesResultsOnError(t *testing.T) {
	for _, mode := range []string{"partial", "decode", "continuation", "transport"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, source, target, issue := setupMoveCLIProjects(t)
			issues := []db.Issue{issue}
			if mode != "partial" {
				for range 49 {
					created, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "other", Author: "tester"})
					require.NoError(t, err)
					issues = append(issues, created)
				}
			}
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
				moved := api.MoveIssuesResult{Ref: issue.UID, Result: "moved", UID: issue.UID, Revision: issue.Revision + 1, EventID: 1}
				if mode == "partial" {
					w.WriteHeader(http.StatusInternalServerError)
					require.NoError(t, json.MarshalWrite(w, map[string]any{"status": 500, "error": map[string]any{"code": "internal", "message": "database unavailable", "data": map[string]any{"results": []api.MoveIssuesResult{moved, {Index: 49, Ref: "invalid!", Result: "refused", Code: "issue_not_found"}}}}}))
					return
				}
				if calls == 1 {
					count := 50
					results := make([]api.MoveIssuesResult, count)
					for j := range results {
						results[j] = moved
						results[j].Index = j
						results[j].Ref, results[j].UID = issues[j].UID, issues[j].UID
						results[j].Revision = issues[j].Revision + 1
						results[j].EventID = int64(j + 1)
					}
					require.NoError(t, json.MarshalWrite(w, map[string]any{"results": results}))
					return
				}
				switch mode {
				case "decode":
					_, err := io.WriteString(w, "{")
					require.NoError(t, err)
				case "continuation":
					// A version-skewed daemon may omit an item outcome.
					_, err := io.WriteString(w, `{"results":[]}`)
					require.NoError(t, err)
				case "transport":
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
				}
			}))
			t.Cleanup(server.Close)
			input := ""
			for _, item := range issues {
				input += item.UID + "\n"
			}
			input += issue.UID + "\n"
			args := []string{"--workspace", dir, "move", target.Name, "--refs-from", "-", "--json"}
			switch mode {
			case "partial":
				input = issue.UID + "\tmissing-project\n" + strings.Repeat(issue.UID+"\t"+target.Name+"\n", 49) + "invalid!\t" + target.Name + "\n"
				args = []string{"--workspace", dir, "move", "--refs-from", "-", "--json"}
			case "transport":
				input = ""
				for _, item := range issues {
					input += item.UID + "\n"
				}
				input += issue.UID + "\n"
			}
			out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, input, args...)
			require.Error(t, err)
			var reply struct {
				Results []api.MoveIssuesResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			if mode == "partial" {
				var ce *cliError
				require.ErrorAs(t, err, &ce)
				require.Equal(t, 500, ce.HTTPStatus)
				require.Len(t, reply.Results, 51)
				require.Equal(t, "refused", reply.Results[0].Result)
				require.Equal(t, "moved", reply.Results[1].Result)
				require.Equal(t, "unknown", reply.Results[2].Result)
				require.Equal(t, "refused", reply.Results[50].Result)
			} else {
				if mode == "transport" {
					var ce *cliError
					require.ErrorAs(t, err, &ce)
					require.Equal(t, 7, ce.ExitCode)
				}
				require.Len(t, reply.Results, 51)
				for j, row := range reply.Results[:50] {
					require.Equal(t, j, row.Index)
					require.Equal(t, "moved", row.Result)
				}
				require.Equal(t, "unknown", reply.Results[50].Result)
			}
		})
	}
}

func TestMoveBatchCLI_HumanShowsEveryResult(t *testing.T) {
	env, dir, src, dst, first := setupMoveCLIProjects(t)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: src.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	args := []string{"--project", src.Name, "move", first.ShortID, second.ShortID, dst.Name}
	out := runCLI(t, env, dir, args...)
	require.Contains(t, out, "ref="+first.ShortID)
	require.Contains(t, out, "ref="+second.ShortID)
	out = runCLI(t, env, dir, args...)
	require.Contains(t, out, "result=unchanged")
	require.Contains(t, out, "ref="+first.ShortID)
	require.Contains(t, out, "ref="+second.ShortID)
}

func TestMoveBatchCLI_InterruptedPagesReportAllInputs(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		t.Run(mode, func(t *testing.T) {
			env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-interruption-token"))
			source, err := env.DB.CreateProject(t.Context(), "source")
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "issue", Author: "tester"})
			require.NoError(t, err)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/issues/actions/move" {
					calls++
					if calls == 2 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusInternalServerError)
						result := api.MoveIssuesResult{Index: 0, Ref: issue.UID, Result: "refused", Code: "claim_denied"}
						require.NoError(t, json.MarshalWrite(w, map[string]any{"status": 500, "error": map[string]any{"code": "internal", "message": "database unavailable", "data": map[string]any{"results": []api.MoveIssuesResult{result}}}}))
						return
					}
				}
				r.Host = origin.Host
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			input := strings.Repeat(issue.UID+"\n", 50) + "\tbad\n" + strings.Repeat(issue.UID+"\n", 51)
			args := []string{"--workspace", dir, "move", target.Name, "--refs-from", "-"}
			if mode != "human" {
				args = append(args, "--"+mode)
			}
			out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, input, args...)
			require.ErrorContains(t, err, "database unavailable")
			require.Equal(t, 2, calls)
			if mode == "json" {
				var reply struct {
					Results []api.MoveIssuesResult `json:"results"`
					Summary struct {
						Unknown     int `json:"unknown"`
						Unattempted int `json:"unattempted"`
						Refused     int `json:"refused"`
					} `json:"summary"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &reply))
				require.Len(t, reply.Results, 102)
				require.Equal(t, "moved", reply.Results[0].Result)
				require.Equal(t, "refused", reply.Results[50].Result)
				require.Equal(t, "claim_denied", reply.Results[51].Code)
				require.Equal(t, "unknown", reply.Results[52].Result)
				require.Equal(t, "unattempted", reply.Results[101].Result)
				require.Equal(t, 49, reply.Summary.Unknown)
				require.Equal(t, 1, reply.Summary.Unattempted)
				require.Equal(t, 2, reply.Summary.Refused)
			} else {
				require.Equal(t, 49, strings.Count(out, "result=unknown"))
				require.Equal(t, 1, strings.Count(out, "result=unattempted"))
				require.Contains(t, out, "claim_denied")
				if mode == "agent" {
					require.Contains(t, out, "refused=2 unknown=49 unattempted=1")
				} else {
					require.Contains(t, out, "2 refused, 49 unknown, 1 unattempted")
				}
			}
		})
	}
}

func TestMoveBatchCLI_RevisionAcrossPages(t *testing.T) {
	env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-revision-token"))
	source, err := env.DB.CreateProject(t.Context(), "source")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "selected", Author: "tester"})
	require.NoError(t, err)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/issues/actions/move" {
			calls++
			if calls == 2 {
				updated, _, changed, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: new("another-worker"), Actor: "tester"})
				require.NoError(t, err)
				require.True(t, changed)
				require.Greater(t, updated.Revision, issue.Revision+1)
			}
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	args := []string{"--workspace", dir, "move"}
	for range 100 {
		args = append(args, issue.UID)
	}
	args = append(args, "--from-project", source.Name, "--status", "all", target.Name, "--json")
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, "", args...)
	require.ErrorContains(t, err, "some issues were refused")
	require.Equal(t, 3, calls)
	var reply struct {
		Results []api.MoveIssuesResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 101)
	require.Equal(t, "moved", reply.Results[0].Result)
	require.Equal(t, "duplicate_item", reply.Results[49].Code)
	require.Equal(t, "already_in_target", reply.Results[50].Code)
	require.Equal(t, "revision_conflict", reply.Results[100].Code)
}

func TestMoveBatchCLI_DifferentTargetsAcrossPages(t *testing.T) {
	env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-targets-token"))
	source, err := env.DB.CreateProject(t.Context(), "source")
	require.NoError(t, err)
	first, err := env.DB.CreateProject(t.Context(), "first")
	require.NoError(t, err)
	second, err := env.DB.CreateProject(t.Context(), "second")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "route", Author: "tester"})
	require.NoError(t, err)
	input := strings.Repeat(issue.UID+"\t"+first.Name+"\n", 50) + strings.Repeat(issue.UID+"\t"+second.Name+"\n", 50) + issue.UID + "\t" + first.Name + "\n"
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), env.URL), t, input, "--workspace", dir, "move", "--refs-from", "-", "--json")
	require.NoError(t, err)
	var reply struct {
		Results []api.MoveIssuesResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 101)
	for _, index := range []int{0, 50, 100} {
		require.Equal(t, "moved", reply.Results[index].Result)
	}
	require.Equal(t, "duplicate_item", reply.Results[99].Code)
	got, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, first.ID, got.ProjectID)
}

func TestMoveBatchCLI_PreviewUIDFromAnotherProject(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), env.URL), t, issue.UID+"\n", "--workspace", dir, "move", target.Name, "--refs-from", "-", "--dry", "--json")
	require.NoError(t, err)
	require.Contains(t, out, issue.UID)
	got, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

func TestMoveBatchCLI_SelectorSubtreeRevision(t *testing.T) {
	for _, option := range []string{"--subtree", "--dry"} {
		t.Run(option, func(t *testing.T) {
			env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-revision-check"))
			source, err := env.DB.CreateProject(t.Context(), "source")
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "selected", Author: "tester"})
			require.NoError(t, err)
			origin, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(origin)
			changed := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !changed && (strings.HasSuffix(r.URL.Path, "/issues/"+issue.UID) || strings.Contains(r.URL.Path, "/actions/move")) {
					changed = true
					updated, _, edited, err := env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: new("another-worker"), Actor: "tester"})
					require.NoError(t, err)
					require.True(t, edited)
					require.Greater(t, updated.Revision, issue.Revision)
				}
				r.Host = origin.Host
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			_, _, err = runCLIAgainst(t, server.URL, dir, "move", "--from-project", source.Name, "--status", "all", option, target.Name)
			var cliErr *cliError
			require.ErrorAs(t, err, &cliErr)
			require.Equal(t, "revision_conflict", cliErr.Code)
			got, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, source.ID, got.ProjectID)
		})
	}
}

func TestMoveBatchCLI_PreviewAlreadyInTarget(t *testing.T) {
	env, dir, source, _, issue := setupMoveCLIProjects(t)
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), env.URL), t, source.Name+"#"+issue.ShortID+"\n", "--workspace", dir, "move", source.Name, "--refs-from", "-", "--dry", "--json")
	require.NoError(t, err)
	require.Contains(t, out, issue.UID)
	require.Contains(t, out, `"result":"unchanged"`)
	require.Contains(t, out, `"ref":"`+source.Name+"#"+issue.ShortID+`"`)
}

func TestMoveBatchCLI_SubtreeCommentFailureShowsMove(t *testing.T) {
	env, dir, _ := setupCLIWorkspaceOptions(t, testenv.WithAuthToken("synthetic-comment-token"))
	source, err := env.DB.CreateProject(t.Context(), "source")
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "tester"})
	require.NoError(t, err)
	origin, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		r.Host = origin.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	out, _, err := executeRootCaptureWithInput(contextWithBaseURL(t.Context(), server.URL), t, source.Name+"#"+issue.ShortID+"\n", "--workspace", dir, "move", target.Name, "--refs-from", "-", "--subtree", "--comment", "routed", "--json")
	require.Error(t, err)
	require.Contains(t, out, issue.UID)
	got, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}

func TestMoveBatchCLI_SelectionBeforeFile(t *testing.T) {
	for _, option := range []string{"--dry", "--subtree"} {
		t.Run(option, func(t *testing.T) {
			_, dir, source, target, issue := setupMoveCLIProjects(t)
			_, _, err := executeRootCaptureWithInput(t.Context(), t, "", "--workspace", dir, "move", source.Name+"#"+issue.ShortID, source.Name+"#"+issue.ShortID, target.Name, "--refs-from", dir+"/missing", option)
			message := "--subtree accepts one issue"
			if option == "--dry" {
				message = "dry run accepts one item"
			}
			require.ErrorContains(t, err, message)
		})
	}
}

func TestMoveBatchCLI_SelectionStopsReadingFile(t *testing.T) {
	for _, option := range []string{"--dry", "--subtree"} {
		t.Run(option, func(t *testing.T) {
			_, dir, source, target, issue := setupMoveCLIProjects(t)
			input := strings.Repeat(source.Name+"#"+issue.ShortID+"\n", 2) + strings.Repeat(strings.Repeat("x", 600000)+"\n", 2)
			_, _, err := executeRootCaptureWithInput(t.Context(), t, input, "--workspace", dir, "move", target.Name, "--refs-from", "-", option)
			message := "--subtree accepts one issue"
			if option == "--dry" {
				message = "dry run accepts one item"
			}
			require.ErrorContains(t, err, message)
		})
	}
}

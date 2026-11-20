package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMoveBatchUnknownStopsNewRowsAndFinishesInflight(t *testing.T) {
	env, _, source, target, first := setupMoveCLIProjects(t)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	third, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "third", Author: "tester"})
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	baseURL := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/actions/move") {
			if strings.Contains(r.URL.Path, first.ShortID) {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			if strings.Contains(r.URL.Path, second.ShortID) {
				close(started)
				<-release
			}
		}
		return false
	})
	a := daemonAPI{ctx: t.Context(), baseURL: baseURL, client: http.DefaultClient}
	client, err := kataclient.NewWithHTTPClient(baseURL, a.client)
	require.NoError(t, err)
	projects, err := loadProjectRefs(a)
	require.NoError(t, err)
	var halt atomic.Bool
	row := func(shortID string) moveBatchRow {
		return moveBatchRow{Ref: source.Name + "#" + shortID, Target: target.Name}
	}
	inflight := moveBatchResult{Result: "unattempted"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		moveBatchOne(a, client, projects, row(second.ShortID), &inflight, "tester", true, &halt)
	}()
	<-started
	unknown := moveBatchResult{Result: "unattempted"}
	moveBatchOne(a, client, projects, row(first.ShortID), &unknown, "tester", true, &halt)
	next := moveBatchResult{Result: "unattempted"}
	moveBatchOne(a, client, projects, row(third.ShortID), &next, "tester", true, &halt)
	close(release)
	<-done
	require.Equal(t, "unknown", unknown.Result)
	require.Equal(t, "unattempted", next.Result)
	require.Equal(t, "moved", inflight.Result)
	stored, err := env.DB.IssueByID(t.Context(), third.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, stored.ProjectID)
}

func TestMoveBatchInvalidSuccessSkipsComment(t *testing.T) {
	env, dir, _, target, issue := setupMoveCLIProjects(t)
	var comments atomic.Int32
	baseURL := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/actions/move") {
			_, _ = io.WriteString(w, "<html>gateway</html>")
			return true
		}
		if strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost {
			comments.Add(1)
		}
		return false
	})
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte(issue.UID+"\n"), 0600))
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--comment", "moved", "--json")
	require.Error(t, err)
	var reply struct {
		Summary struct {
			Moved   int `json:"moved"`
			Unknown int `json:"unknown"`
		} `json:"summary"`
		Results []moveBatchResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Equal(t, 0, reply.Summary.Moved)
	require.Equal(t, 1, reply.Summary.Unknown)
	require.Len(t, reply.Results, 1)
	require.Contains(t, reply.Results[0].Hint, "outcome unconfirmed")
	require.Zero(t, comments.Load())
}

func TestMoveBatchMovesOnlyListedIssues(t *testing.T) {
	env, dir, source, target, first := setupMoveCLIProjects(t)
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "tester", Links: []db.InitialLink{{Type: "parent", ToNumber: first.ID}}})
	require.NoError(t, err)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), interceptingProxy(t, env, func(_ http.ResponseWriter, _ *http.Request) bool { return false })), "--workspace", dir, "move", first.UID, second.UID, target.Name, "--json")
	require.NoError(t, err, out)
	require.Contains(t, out, `"moved":2`)
	got, err := env.DB.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

func TestMoveBatchRefsFromFile(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte(source.Name+"#"+issue.ShortID+"\t"+target.Name+"\n"), 0600))
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "move", "--refs-from", path, "--json")
	require.NoError(t, err, out)
	var reply struct {
		Results []moveBatchResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 1)
	require.Equal(t, "moved", reply.Results[0].Result)
	got, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}

func TestMoveBatchLostResponseUsesUIDAndHistory(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	upstream, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }, ModifyResponse: func(r *http.Response) error {
		if strings.HasSuffix(r.Request.URL.Path, "/actions/move") {
			return fmt.Errorf("drop response")
		}
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		c, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			_ = c.Close()
		}
	}}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte(source.Name+"#"+issue.ShortID+"\n"), 0600))
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--json")
	require.Error(t, err)
	var reply struct {
		Results []moveBatchResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 1)
	require.Equal(t, "unknown", reply.Results[0].Result)
	require.Equal(t, issue.UID, reply.Results[0].UID)
	require.Equal(t, target.Name, reply.Results[0].Target)
	require.Contains(t, reply.Results[0].Hint, "Check move history before retrying if the issue may have moved more than once")
	require.NotContains(t, reply.Results[0].Hint, "safe to retry")
	for _, workspace := range []string{t.TempDir(), dir} {
		shown, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", workspace, "show", issue.UID, "--json")
		require.NoError(t, err, shown)
		var result struct {
			Issue struct {
				ProjectID int64 `json:"project_id"`
			} `json:"issue"`
		}
		require.NoError(t, json.Unmarshal([]byte(shown), &result))
		require.Equal(t, target.ID, result.Issue.ProjectID)
	}
	require.Contains(t, reply.Results[0].Hint, fmt.Sprintf("target project ID: %d", target.ID))
	require.Contains(t, reply.Results[0].Hint, fmt.Sprintf("Source project ID: %d", source.ID))
}

func TestMoveBatchCancelMarksOnlyDispatchedMovesUnknown(t *testing.T) {
	env, dir, source, target, first := setupMoveCLIProjects(t)
	refs := []string{first.UID}
	for range 11 {
		issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "issue", Author: "tester"})
		require.NoError(t, err)
		refs = append(refs, issue.UID)
	}
	started := make(chan struct{}, 12)
	release := make(chan struct{})
	baseURL := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/actions/move") {
			started <- struct{}{}
			<-release
			return true
		}
		return false
	})
	ctx, cancel := context.WithCancel(contextWithBaseURL(t.Context(), baseURL))
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-started:
			cancel()
		case <-done:
		}
	}()
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(refs, "\n")), 0600))
	out, _, err := executeRootCapture(t, ctx, "--workspace", dir, "move", target.Name, "--refs-from", path, "--json")
	close(done)
	close(release)
	require.Error(t, err)
	var reply struct {
		Results []moveBatchResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, len(refs))
	var unknown, unattempted int
	for _, r := range reply.Results {
		switch r.Result {
		case "unknown":
			unknown++
		case "unattempted":
			unattempted++
			require.Equal(t, "not sent; safe to rerun", r.Hint)
		default:
			t.Fatalf("unexpected result: %+v", r)
		}
	}
	require.Positive(t, unknown)
	require.Positive(t, unattempted)
}

func TestMoveBatchCommentRetryUsesOfficialTarget(t *testing.T) {
	env, dir, _, target, issue := setupMoveCLIProjects(t)
	target, err := env.DB.RenameProject(t.Context(), target.ID, "target project")
	require.NoError(t, err)
	baseURL := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return true
		}
		return false
	})
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte(issue.UID+"\n"), 0600))
	_, _, err = executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), "--workspace", dir, "move", strconv.FormatInt(target.ID, 10), "--refs-from", path, "--comment", "routed")
	require.Error(t, err)
	require.Contains(t, err.Error(), "kata comment "+issue.UID+" --project '"+target.Name+"' --body ...")
}

func TestMoveBatchFileParsingLimit(t *testing.T) {
	rows, err := parseRefsFrom(strings.NewReader("\nsource#abc4\nsource#def5\tother\n"), "target")
	require.NoError(t, err)
	require.Equal(t, []moveBatchRow{{Ref: "source#abc4", Target: "target"}, {Ref: "source#def5", Target: "other"}}, rows)
	input := strings.Repeat("source#abc4\n", 5001) + strings.Repeat("x", 100000)
	reader := strings.NewReader(input)
	_, err = parseRefsFrom(reader, "target")
	require.ErrorContains(t, err, "at most 5000 items")
	require.Less(t, len(input)-reader.Len(), len(input)/2)
}

func TestMoveBatchGatewayAndResponseOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"gateway 502", 502, "bad gateway", "unknown"},
		{"gateway 503", 503, "service unavailable", "unknown"},
		{"gateway 504", 504, "gateway timeout", "unknown"},
		{"kata refusal", 503, `{"error":{"code":"unavailable","message":"try later"}}`, "refused"},
		{"malformed success", 200, `<html>gateway</html>`, "unknown"},
		{"empty success", 200, `{}`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, _, target, issue := setupMoveCLIProjects(t)
			baseURL := ""
			if tc.name == "kata refusal" {
				baseURL = interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
					if strings.HasSuffix(r.URL.Path, "/actions/move") {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(tc.body))
						return true
					}
					return false
				})
			} else {
				upstream, err := url.Parse(env.URL)
				require.NoError(t, err)
				proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }, ModifyResponse: func(r *http.Response) error {
					if strings.HasSuffix(r.Request.URL.Path, "/actions/move") {
						_ = r.Body.Close()
						r.StatusCode = tc.status
						r.Body = io.NopCloser(strings.NewReader(tc.body))
						r.ContentLength = int64(len(tc.body))
						r.Header.Set("Content-Length", strconv.Itoa(len(tc.body)))
					}
					return nil
				}}
				server := httptest.NewServer(proxy)
				t.Cleanup(server.Close)
				baseURL = server.URL
			}
			path := filepath.Join(t.TempDir(), "refs")
			require.NoError(t, os.WriteFile(path, []byte(issue.UID+"\n"), 0600))
			out, _, _ := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--json")
			var reply struct {
				Results []moveBatchResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			require.Len(t, reply.Results, 1)
			require.Equal(t, tc.want, reply.Results[0].Result)
			require.Equal(t, issue.UID, reply.Results[0].UID)
			require.Equal(t, target.Name, reply.Results[0].Target)
			if tc.name != "kata refusal" {
				stored, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, target.ID, stored.ProjectID)
			}
			if tc.status == 200 {
				require.Contains(t, reply.Results[0].Hint, "outcome unconfirmed")
			}
		})
	}
}

func TestMoveBatchLookupTransportFailureIsUnattempted(t *testing.T) {
	env, dir, _, target, issue := setupMoveCLIProjects(t)
	baseURL := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v1/issues/"+issue.UID {
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = c.Close()
			}
			return true
		}
		return false
	})
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte(issue.UID+"\n"), 0600))
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--json")
	require.Error(t, err)
	var reply struct {
		Results []moveBatchResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 1)
	require.Equal(t, "unattempted", reply.Results[0].Result)
	require.NotEmpty(t, reply.Results[0].Message)
	require.Equal(t, "not sent; safe to rerun", reply.Results[0].Hint)
}

func TestMoveBatchCommentFailureVisibleInRowAndQuiet(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(strconv.FormatBool(quiet), func(t *testing.T) {
			env, dir, _, target, issue := setupMoveCLIProjects(t)
			baseURL := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
					http.Error(w, "unavailable", http.StatusInternalServerError)
					return true
				}
				return false
			})
			path := filepath.Join(t.TempDir(), "refs")
			require.NoError(t, os.WriteFile(path, []byte(issue.UID+"\ninvalid\n"), 0600))
			args := []string{"--workspace", dir, "move", strconv.FormatInt(target.ID, 10), "--refs-from", path, "--comment", "routed"}
			if quiet {
				args = append(args, "--quiet")
			} else {
				args = append(args, "--json")
			}
			out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), args...)
			require.Error(t, err)
			retry := "kata comment " + issue.UID + " --project " + target.Name + " --body ..."
			if quiet {
				require.Contains(t, stderr, retry)
			} else {
				var reply struct {
					Results []moveBatchResult `json:"results"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &reply))
				require.Equal(t, "moved", reply.Results[0].Result)
				require.Contains(t, reply.Results[0].Message, "comment")
				require.Contains(t, reply.Results[0].Hint, retry)
			}
		})
	}
}

func TestMoveBatchCLI_OutputModes(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(strconv.FormatBool(partial), func(t *testing.T) {
			env, dir, _, target, issue := setupMoveCLIProjects(t)
			input := issue.UID + "\n"
			if partial {
				input += "invalid\n"
			}
			path := filepath.Join(t.TempDir(), "refs")
			require.NoError(t, os.WriteFile(path, []byte(input), 0600))
			out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--quiet")
			if partial {
				require.Error(t, err)
				require.Contains(t, out, "1 moved, 1 refused")
				require.Contains(t, out, "result=refused")
			} else {
				require.NoError(t, err)
				require.Empty(t, out)
			}
		})
	}
}

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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
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
	unknown := moveBatchResult{Result: "unattempted"}
	next := moveBatchResult{Result: "unattempted"}
	preparedSecond := resolveMoveBatchOne(a, client, projects, row(second.ShortID), &inflight)
	preparedFirst := resolveMoveBatchOne(a, client, projects, row(first.ShortID), &unknown)
	preparedThird := resolveMoveBatchOne(a, client, projects, row(third.ShortID), &next)
	require.NotNil(t, preparedSecond)
	require.NotNil(t, preparedFirst)
	require.NotNil(t, preparedThird)
	done := make(chan struct{})
	go func() {
		defer close(done)
		moveBatchOne(a, client, preparedSecond, &inflight, "tester", true, &halt)
	}()
	<-started
	moveBatchOne(a, client, preparedFirst, &unknown, "tester", true, &halt)
	moveBatchOne(a, client, preparedThird, &next, "tester", true, &halt)
	close(release)
	<-done
	require.Equal(t, "unknown", unknown.Result)
	require.Equal(t, "unattempted", next.Result)
	require.Equal(t, "moved", inflight.Result)
	stored, err := env.DB.IssueByID(t.Context(), third.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, stored.ProjectID)
}

func TestMoveBatchIncompleteSuccessRemainsUnknown(t *testing.T) {
	env, dir, _, target, issue := setupMoveCLIProjects(t)
	var comments atomic.Int32
	baseURL := interceptingProxy(t, env, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/move") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"issue":{"short_id":"next"}}`)
			return true
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			comments.Add(1)
		}
		return false
	})
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte(issue.UID+"\n"), 0600))
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--comment", "routed", "--json")
	assert.Error(t, err)
	var reply struct {
		Results []moveBatchResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &reply))
	require.Len(t, reply.Results, 1)
	assert.Equal(t, "unknown", reply.Results[0].Result)
	assert.Zero(t, comments.Load())
}

func TestMoveBatchMovesOnlyListedIssues(t *testing.T) {
	env, dir, source, target, first := setupMoveCLIProjects(t)
	child, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "tester", Links: []db.InitialLink{{Type: "parent", ToNumber: first.ID}}})
	require.NoError(t, err)
	second, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "move", first.UID, second.UID, target.Name, "--json")
	require.NoError(t, err, out)
	require.Contains(t, out, `"moved":2`)
	got, err := env.DB.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
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
	baseURL := interceptingProxy(t, env, func(_ http.ResponseWriter, r *http.Request) bool {
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
		name   string
		status int
		body   string
	}{
		{"lost response", 0, ""},
		{"gateway envelope", 500, `{"error":{"code":"unavailable","message":"try later"}}`},
		{"malformed success", 200, `<html>gateway</html>`},
		{"empty success", 200, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, source, target, issue := setupMoveCLIProjects(t)
			upstream, err := url.Parse(env.URL)
			require.NoError(t, err)
			var comments atomic.Int32
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
				if strings.HasSuffix(r.In.URL.Path, "/comments") {
					comments.Add(1)
				}
				r.SetURL(upstream)
			}, ModifyResponse: func(r *http.Response) error {
				if strings.HasSuffix(r.Request.URL.Path, "/actions/move") {
					if tc.status == 0 {
						return fmt.Errorf("drop response")
					}
					_ = r.Body.Close()
					r.StatusCode = tc.status
					r.Body = io.NopCloser(strings.NewReader(tc.body))
					r.ContentLength = int64(len(tc.body))
					r.Header.Set("Content-Length", strconv.Itoa(len(tc.body)))
				}
				return nil
			}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = c.Close()
				}
			}}
			server := httptest.NewServer(proxy)
			t.Cleanup(server.Close)
			path := filepath.Join(t.TempDir(), "refs")
			require.NoError(t, os.WriteFile(path, []byte(source.Name+"#"+issue.ShortID+"\n"), 0600))
			out, _, _ := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--comment", "moved", "--json")
			var reply struct {
				Results []moveBatchResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			require.Len(t, reply.Results, 1)
			stored, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, stored.ProjectID)
			require.Equal(t, "unknown", reply.Results[0].Result)
			require.Equal(t, issue.UID, reply.Results[0].UID)
			require.Equal(t, target.Name, reply.Results[0].Target)
			require.Contains(t, reply.Results[0].Hint, "outcome unconfirmed")
			require.Contains(t, reply.Results[0].Hint, fmt.Sprintf("Source project ID: %d; target project ID: %d", source.ID, target.ID))
			require.Contains(t, reply.Results[0].Hint, "Check move history before retrying")
			require.Zero(t, comments.Load())
		})
	}
}

func TestMoveBatchDuplicateResolvedUID(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(strconv.FormatBool(conflict), func(t *testing.T) {
			env, dir, source, target, issue := setupMoveCLIProjects(t)
			other, err := env.DB.CreateProject(t.Context(), "other-project")
			require.NoError(t, err)
			secondTarget := target.Name
			if conflict {
				secondTarget = other.Name
			}
			var posts atomic.Int32
			baseURL := interceptingProxy(t, env, func(_ http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/actions/move") {
					posts.Add(1)
				}
				return false
			})
			path := filepath.Join(t.TempDir(), "refs")
			input := issue.UID + "\t" + target.Name + "\n" + source.Name + "#" + issue.ShortID + "\t" + secondTarget + "\n"
			require.NoError(t, os.WriteFile(path, []byte(input), 0600))
			out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), "--workspace", dir, "move", "--refs-from", path, "--json")
			require.Error(t, err)
			var reply struct {
				Results []moveBatchResult `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &reply))
			require.Len(t, reply.Results, 2)
			stored, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			if conflict {
				require.Zero(t, posts.Load())
				require.Equal(t, source.ID, stored.ProjectID)
				require.Equal(t, "refused", reply.Results[0].Result)
				require.Equal(t, "refused", reply.Results[1].Result)
			} else {
				require.EqualValues(t, 1, posts.Load())
				require.Equal(t, target.ID, stored.ProjectID)
				require.Equal(t, "moved", reply.Results[0].Result)
				require.Equal(t, "refused", reply.Results[1].Result)
			}
		})
	}
}

func TestMoveBatchRefusalOutputErrors(t *testing.T) {
	env, dir, _, target, _ := setupMoveCLIProjects(t)
	path := filepath.Join(t.TempDir(), "refs")
	require.NoError(t, os.WriteFile(path, []byte("invalid\n"), 0600))
	args := []string{"--workspace", dir, "move", target.Name, "--refs-from", path}
	resetFlags(t)
	cmd := newRootCmd()
	cmd.SetOut(cliSearchErrorWriter{})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(contextWithBaseURL(t.Context(), env.URL))
	require.ErrorIs(t, err, errCLISearchWrite)
	var stderr bytes.Buffer
	emitRootError(&stderr, cmd, args, err, runEEntered)
	require.Contains(t, stderr.String(), errCLISearchWrite.Error())
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
			require.NoError(t, os.WriteFile(path, []byte(issue.UID+"\ninvalid\n"), 0600))
			args := []string{"--workspace", dir, "move", strconv.FormatInt(target.ID, 10), "--refs-from", path, "--comment", "routed"}
			if quiet {
				args = append(args, "--quiet")
			} else {
				args = append(args, "--json")
			}
			out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), baseURL), args...)
			require.Error(t, err)
			retry := "kata comment " + issue.UID + " --project '" + target.Name + "' --body ..."
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
			out, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "move", target.Name, "--refs-from", path, "--quiet")
			require.Empty(t, stderr)
			if partial {
				require.Equal(t, ExitConflict, exitCodeForErr(err, true))
				require.Contains(t, out, "1 moved, 1 refused")
				require.Contains(t, out, "result=refused")
			} else {
				require.NoError(t, err)
				require.Empty(t, out)
			}
		})
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/transcript"
)

func transcriptProjectConfig(t *testing.T, text string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_TRANSCRIPT_AGENT", "codex")
	t.Setenv("KATA_TRANSCRIPT_SESSION_ID", transcriptSessionOne)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(text), 0600))
}

func closeReceiptTranscript(t *testing.T, output string) *transcript.Transcript {
	t.Helper()
	var receipt api.MutationResponse
	require.NoError(t, json.Unmarshal([]byte(output), &receipt.Body))
	require.NotNil(t, receipt.Body.Event)
	var payload struct {
		Transcript *transcript.Transcript `json:"transcript"`
	}
	require.NoError(t, json.Unmarshal([]byte(receipt.Body.Event.Payload), &payload))
	return payload.Transcript
}

func TestCloseTranscriptProjectSelection(t *testing.T) {
	for _, selector := range []string{"workspace", "flag", "qualified", "moved", "ulid"} {
		t.Run(selector, func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Complete project work")
			team, err := env.DB.CreateProject(t.Context(), "team-project")
			require.NoError(t, err)
			args := []string{}
			project := "team-project"
			switch selector {
			case "workspace":
				project = "kata"
			case "flag":
				ref = createIssue(t, env, team.ID, "Team work")
				args = []string{"--project", team.Name}
			case "qualified":
				ref = team.Name + "#" + createIssue(t, env, team.ID, "Qualified work")
				args = []string{"--project", "kata"}
			case "moved":
				runCLI(t, env, dir, "move", ref, team.Name)
				ref = "kata#" + ref
			case "ulid":
				issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
				require.NoError(t, err)
				ref = issue.UID
				project = "kata"
			}
			transcriptProjectConfig(t, `[close.transcript]
enabled=true
agentsview_url="https://default.example"
[close.transcript.project.kata]
agentsview_url="https://workspace.example"
[close.transcript.project.team-project]
agentsview_url="https://team.example/archive"
`)
			args = append(args, "--json", "close", ref, "--done", "--message", "Implemented project behavior and checked the focused tests.", "--test", "go test ./internal/example")
			out, stderr, err := runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			require.Empty(t, stderr)
			got := closeReceiptTranscript(t, out)
			require.NotNil(t, got)
			base := "https://team.example/archive"
			if project == "kata" {
				base = "https://workspace.example"
			}
			assert.Equal(t, base+"/sessions/codex/"+transcriptSessionOne, got.URL)
		})
	}
}

func TestCloseTranscriptProjectLookupFailure(t *testing.T) {
	for _, override := range []string{"", "true"} {
		t.Run("override="+override, func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete example work")
			transcriptProjectConfig(t, "[close.transcript]\nenabled=true\n[close.transcript.project.kata]\nenabled=false\n")
			t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", override)
			upstream, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/"+ref) {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			env.URL = server.URL
			out, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example")
			require.NoError(t, err)
			got := closeReceiptTranscript(t, out)
			if override == "true" {
				require.NotNil(t, got)
				assert.Empty(t, got.URL)
			} else {
				assert.Nil(t, got)
			}
			assert.Contains(t, stderr, "project unavailable")
		})
	}
}

func TestCloseTranscriptProjectKeyedRetry(t *testing.T) {
	for _, change := range []string{"deleted", "moved"} {
		t.Run(change, func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Complete example work")
			transcriptProjectConfig(t, `[close.transcript]
enabled=true
[close.transcript.project.kata]
agentsview_url="https://first.example"
[close.transcript.project.other]
enabled=false
agentsview_url="https://second.example"
`)
			args := []string{"--json", "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example", "--idempotency-key", "project-retry"}
			out, _, err := runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			require.NotNil(t, closeReceiptTranscript(t, out))
			issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
			require.NoError(t, err)
			if change == "deleted" {
				_, _, _, err = env.DB.SoftDeleteIssue(t.Context(), issue.ID, "tester")
			} else {
				var target db.Project
				target, err = env.DB.CreateProject(t.Context(), "other")
				require.NoError(t, err)
				_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: pid, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			}
			require.NoError(t, err)
			out, _, err = runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			assert.Contains(t, out, `"reused":true`)
		})
	}
}

func TestCloseTranscriptProjectNameLookupFailure(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete example work")
	team, err := env.DB.CreateProject(t.Context(), "team-project")
	require.NoError(t, err)
	runCLI(t, env, dir, "move", ref, team.Name)
	transcriptProjectConfig(t, "[close.transcript]\nenabled=true\n[close.transcript.project.team-project]\nenabled=false\n")
	upstream, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("/api/v1/projects/%d", team.ID) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	env.URL = server.URL
	out, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", "kata#"+ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example")
	require.NoError(t, err)
	assert.Nil(t, closeReceiptTranscript(t, out))
	assert.Contains(t, stderr, "project unavailable")
}

func TestCloseTranscriptProjectMoveBeforePost(t *testing.T) {
	for _, record := range []bool{false, true} {
		t.Run(fmt.Sprintf("record=%t", record), func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Complete example work")
			issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
			require.NoError(t, err)
			target, err := env.DB.CreateProject(t.Context(), "other")
			require.NoError(t, err)
			transcriptProjectConfig(t, fmt.Sprintf("[close.transcript]\nenabled=true\n[close.transcript.project.kata]\nenabled=%t\n", record))
			upstream, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/close") {
					_, moveErr := env.DB.MoveIssueProject(r.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: pid, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
					if moveErr != nil {
						http.Error(w, moveErr.Error(), http.StatusInternalServerError)
						return
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			env.URL = server.URL
			_, _, err = runCLIWithErr(t, env, dir, "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "moved")
			current, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			assert.Equal(t, "open", current.Status)
		})
	}
}

func TestCloseTranscriptProjectRetryAfterLookupFailure(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail-first=%t", failFirst), func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete example work")
			transcriptProjectConfig(t, "[close.transcript]\nenabled=true\n[close.transcript.project.kata]\nagentsview_url=\"https://team.example\"\n")
			upstream, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
			var fail atomic.Bool
			fail.Store(failFirst)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/"+ref) {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			env.URL = server.URL
			args := []string{"--json", "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example", "--idempotency-key", "lookup-retry"}
			out, _, err := runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			if failFirst {
				assert.Nil(t, closeReceiptTranscript(t, out))
			} else {
				require.NotNil(t, closeReceiptTranscript(t, out))
			}
			fail.Store(!failFirst)
			out, _, err = runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			assert.Contains(t, out, `"reused":true`)
		})
	}
}

func TestCloseTranscriptProjectRetryAfterCommentFailure(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Complete example work")
	transcriptProjectConfig(t, "[close.transcript]\nenabled=true\n[close.transcript.project.kata]\nagentsview_url=\"https://team.example\"\n")
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	upstream, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream) }}
	var failComment atomic.Bool
	failComment.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") && failComment.Swap(false) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	env.URL = server.URL
	args := []string{"--json", "close", ref, "--done", "--message", "Implemented behavior and checked the focused example tests.", "--test", "go test ./internal/example", "--comment", "completed", "--idempotency-key", "comment-retry"}
	_, _, err = runCLIWithErr(t, env, dir, args...)
	require.Error(t, err)
	out, _, err := runCLIWithErr(t, env, dir, args...)
	require.NoError(t, err)
	assert.Contains(t, out, `"reused":true`)
	comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "completed", comments[0].Body)
}

func TestCloseTranscriptProjectOverride(t *testing.T) {
	for _, tc := range []struct {
		name, entry    string
		global, attach bool
		url            string
	}{
		{"inherit", `agentsview_url="https://team.example"`, true, true, "https://team.example/sessions/codex/" + transcriptSessionOne},
		{"enable", `enabled=true`, false, true, ""},
		{"disable", `enabled=false`, true, false, ""},
		{"blank URL", `agentsview_url=""`, true, true, ""},
		{"invalid URL", `agentsview_url="https://user:secret@team.example"`, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, _, ref := setupWorkspaceWithIssue(t, "Complete example work")
			transcriptProjectConfig(t, fmt.Sprintf("[close.transcript]\nenabled=%t\nagentsview_url=\"https://default.example\"\n[close.transcript.project.kata]\n%s\n", tc.global, tc.entry))
			out, stderr, err := runCLIWithErr(t, env, dir, "--json", "close", ref, "--done", "--message", "Implemented behavior and ran the example package tests.", "--test", "go test ./internal/example")
			require.NoError(t, err)
			got := closeReceiptTranscript(t, out)
			if tc.attach {
				require.NotNil(t, got)
				assert.Equal(t, tc.url, got.URL)
			} else {
				assert.Nil(t, got)
			}
			assert.NotContains(t, stderr, "secret")
		})
	}
}

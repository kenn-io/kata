package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestTeammateCLICommentReadback(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	t.Setenv("KATA_TEAMMATE", "reviewer-7")
	short := createIssue(t, env, pid, "Existing issue")
	runCLI(t, env, dir, "--as", "coordinator", "comment", short, "--body", "check retries")
	got := fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, got.Comments, 1)
	require.Equal(t, "reviewer-7", got.Comments[0].Teammate)
	require.Equal(t, "coordinator", got.Comments[0].Author)
	runCLI(t, env, dir, "--teammate=", "comment", short, "--body", "no attribution")
	runCLI(t, env, dir, "--teammate", "implementer-3", "edit", short, "--title", "Updated", "--comment", "implemented")
	got = fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, got.Comments, 3)
	require.Empty(t, got.Comments[1].Teammate)
	require.Equal(t, "implementer-3", got.Comments[2].Teammate)
}

func TestTeammateCLICreateMetadata(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	t.Setenv("KATA_TEAMMATE", "reviewer-7")
	for _, tc := range []struct {
		name string
		args []string
		want string
		fail bool
	}{
		{"inherited", nil, "reviewer-7", false},
		{"override", []string{"--teammate", "implementer-3"}, "implementer-3", false},
		{"empty", []string{"--teammate="}, "", false},
		{"matching", []string{"--meta", "teammate=reviewer-7"}, "reviewer-7", false},
		{"conflict", []string{"--meta", "teammate=implementer-3"}, "", true},
		{"explicit metadata", []string{"--teammate=", "--meta", "teammate=implementer-3"}, "implementer-3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--json", "--as", "coordinator", "create", "Issue " + tc.name, "--force-new"}, tc.args...)
			out, err := runCLICapture(t, env, dir, args...)
			if tc.fail {
				require.ErrorContains(t, err, "teammate")
				return
			}
			require.NoError(t, err)
			var got struct {
				Issue struct {
					Author   string         `json:"author"`
					Owner    *string        `json:"owner"`
					Metadata map[string]any `json:"metadata"`
				} `json:"issue"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			require.Equal(t, "coordinator", got.Issue.Author)
			require.Nil(t, got.Issue.Owner)
			if tc.want == "" {
				require.NotContains(t, got.Issue.Metadata, "teammate")
			} else {
				require.Equal(t, tc.want, got.Issue.Metadata["teammate"])
			}
		})
	}
}

func TestTeammateFollowupRejectsBeforePrimaryMutation(t *testing.T) {
	for _, handle := range []string{"reviewer-7", "bad/handle"} {
		t.Run(handle, func(t *testing.T) {
			t.Setenv("KATA_TEAMMATE", handle)
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.17.0"}`))
					return
				}
				if r.URL.Path == "/api/v1/projects/resolve" {
					_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
					return
				}
				if r.Method != "GET" {
					writes.Add(1)
				}
				_, _ = w.Write([]byte(`{"issue":{"id":1,"project_id":1,"short_id":"abc1"},"changed":true}`))
			}))
			t.Cleanup(server.Close)
			for _, action := range [][]string{
				{"claim", "abc1"}, {"assign", "abc1", "coordinator"}, {"unassign", "abc1"},
				{"edit", "abc1", "--title", "Updated"}, {"label", "add", "abc1", "example-label"}, {"label", "rm", "abc1", "example-label"},
				{"close", "abc1", "--done", "--message", "Implemented requested behavior and verified the focused tests.", "--test", "go test ./cmd/kata"},
				{"reopen", "abc1"}, {"move", "abc1", "target-project"},
			} {
				args := append([]string{"--project", "example-project"}, action...)
				args = append(args, "--comment", "claiming")
				_, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL), args...)
				require.Error(t, err, "%v", action)
				require.Contains(t, err.Error(), "teammate", "%v", action)
				require.Zero(t, writes.Load(), "%v", action)
			}
		})
	}
}

func TestTeammateCLIFollowupCommands(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	t.Setenv("KATA_TEAMMATE", "reviewer-7")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"claim", []string{"claim"}}, {"assign", []string{"assign"}}, {"unassign", []string{"unassign"}},
		{"edit", []string{"edit"}}, {"label add", []string{"label", "add"}}, {"label rm", []string{"label", "rm"}},
		{"close", []string{"close"}}, {"reopen", []string{"reopen"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := createIssue(t, env, pid, "Work "+tc.name)
			args := append(append([]string{}, tc.args...), ref)
			switch tc.name {
			case "assign":
				args = append(args, "coordinator")
			case "unassign":
				runCLI(t, env, dir, "assign", ref, "coordinator")
			case "edit":
				args = append(args, "--title", "Revised")
			case "label add":
				args = append(args, "example-label")
			case "label rm":
				runCLI(t, env, dir, "label", "add", ref, "example-label")
				args = append(args, "example-label")
			case "close":
				args = append(args, "--done", "--message", "Implemented requested behavior and verified the focused test suite.", "--test", "go test ./cmd/kata")
			case "reopen":
				runCLI(t, env, dir, "close", ref, "--done", "--message", "Implemented requested behavior and verified the focused test suite.", "--test", "go test ./cmd/kata")
			}
			args = append(args, "--comment", "check retries")
			runCLI(t, env, dir, args...)
			got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
			require.Len(t, got.Comments, 1)
			require.Equal(t, "reviewer-7", got.Comments[0].Teammate)
		})
	}
}

func TestTeammateCLICloseRetryAfterMove(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Retry work")
	t.Setenv("KATA_TEAMMATE", "reviewer-7")
	args := []string{"close", ref, "--done", "--message", "Implemented requested behavior and verified the focused test suite.", "--test", "go test ./cmd/kata", "--comment", "check retries", "--idempotency-key", "teammate-close-retry"}
	runCLI(t, env, dir, args...)
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	target, err := env.DB.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: pid, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "coordinator"})
	require.NoError(t, err)
	runCLI(t, env, dir, args...)
	t.Setenv("KATA_TEAMMATE", "implementer-3")
	_, err = runCLICapture(t, env, dir, args...)
	require.ErrorContains(t, err, "different fingerprint")
	comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "reviewer-7", comments[0].Teammate)
}

func TestTeammateCLIMoveFollowup(t *testing.T) {
	env, dir, source, target, issue := setupMoveCLIProjects(t)
	t.Setenv("KATA_TEAMMATE", "reviewer-7")
	runCLI(t, env, dir, "--project", source.Name, "move", issue.ShortID, target.Name, "--comment", "relocated")
	comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "reviewer-7", comments[0].Teammate)
}

func TestTeammateInvalidDefaultDoesNotAffectReadsOrUnattributedEdits(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Existing issue")
	t.Setenv("KATA_TEAMMATE", "bad/handle")
	runCLI(t, env, dir, "show", ref)
	runCLI(t, env, dir, "edit", ref, "--title", "Updated")
	runCLI(t, env, dir, "--teammate=", "comment", ref, "--body", "No attribution")
	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Equal(t, "Updated", got.Issue.Title)
	require.Len(t, got.Comments, 1)
	require.Empty(t, got.Comments[0].Teammate)
	_, err := runCLICapture(t, env, dir, "mcp", "serve")
	require.ErrorContains(t, err, "teammate")
}

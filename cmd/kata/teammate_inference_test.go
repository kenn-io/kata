package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/diagnostics"
	"go.kenn.io/kata/internal/harness"
	"go.kenn.io/kata/internal/notification"
)

func unsetTeammateEnv(t *testing.T, key string) {
	t.Helper()
	previous, present := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, previous)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestInferredTypedReplyKeepsActorAddressingAcrossIssues(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi", Model: "example"}, harness.Detected)

	requestIssue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Request", Author: "coordinator"})
	require.NoError(t, err)
	answerIssue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Answer", Author: "worker"})
	require.NoError(t, err)
	target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: answerIssue.ID, Author: "reviewer", Body: "Finding"})
	require.NoError(t, err)
	runCLIAs(t, env, dir, "coordinator", "notify", requestIssue.ShortID, "--to", "worker", "--re", target.UID, "--message", "Please answer this finding")
	runCLIAs(t, env, dir, "worker", "comment", answerIssue.ShortID, "--reply", target.UID, "--body", "I checked the requested finding and confirmed the result.")

	current, err := env.DB.IssueByID(t.Context(), requestIssue.ID)
	require.NoError(t, err)
	var slots map[string]notification.Value
	require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
	require.NotContains(t, slots, notification.MetadataKey("worker"), "inference must not move the reply to a teammate inbox")
	answered, ok := slots[notification.MetadataKey("coordinator")]
	require.True(t, ok, "the requester learns the answer on the issue where they asked")
	require.Equal(t, "worker", answered.From)
	require.Equal(t, "reply", answered.Kind)

	got := fetchIssueViaHTTPWithComments(t, env, pid, answerIssue.ShortID)
	var answerTeammate string
	var answerUID string
	for _, comment := range got.Comments {
		if comment.Body == "I checked the requested finding and confirmed the result." {
			answerTeammate = comment.Teammate
			answerUID = comment.UID
		}
	}
	require.Equal(t, "pi.example", answerTeammate, "stored attribution follows the inferred request header while reply addressing remains actor-only")
	require.NotEmpty(t, answerUID)
	require.Equal(t, answerUID, answered.Re)
}

func TestDoctorInvalidTeammateInferenceConfig(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[teammate]\ninfer='maybe'\n"), 0600))
	flags = globalFlags{Workspace: workspace}
	finding := doctorFinding(t, collectDoctor(context.Background()), "config.daemon")
	require.Equal(t, diagnostics.StatusFail, finding.Status)
	require.True(t, strings.Contains(finding.Summary, "teammate"), finding.Summary)
}

func TestInferredCommentUsesHeaderOnOldDaemon(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi", Model: "example"}, harness.Detected)
	var bodies []map[string]any
	var closes, health int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/health":
			health++
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.17.0"}`))
		case r.URL.Path == "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			require.Equal(t, "pi.example", r.Header.Get("X-Kata-Teammate"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			bodies = append(bodies, body)
			_, _ = w.Write([]byte(`{"comment":{"id":1,"uid":"01AAAAAAAAAAAAAAAAAAAAAAAA","body":"progress","author":"tester","created_at":"2030-01-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			closes++
			_, _ = w.Write([]byte(`{"issue":{"id":1,"project_id":1,"short_id":"abc1","title":"Existing","revision":2},"changed":true}`))
		default:
			_, _ = w.Write([]byte(`{"issue":{"id":1,"project_id":1,"short_id":"abc1","title":"Existing","revision":1}}`))
		}
	}))
	t.Cleanup(server.Close)
	_, stderr, err := executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL), "--project", "example-project", "comment", "abc1", "--body", "progress")
	require.NoError(t, err)
	require.Len(t, bodies, 1)
	require.NotContains(t, bodies[0], "teammate")
	require.Empty(t, stderr)
	require.Zero(t, health)
	_, stderr, err = executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL), "--project", "example-project", "close", "abc1", "--done", "--message", "Implemented requested behavior and verified the focused tests.", "--test", "focused checks", "--comment", "progress")
	require.NoError(t, err)
	require.Equal(t, 1, closes)
	require.Len(t, bodies, 2)
	require.NotContains(t, bodies[1], "teammate")
	require.Empty(t, stderr)
	require.Zero(t, health)
	_, _, err = executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL), "--teammate", "explicit", "--project", "example-project", "comment", "abc1", "--body", "progress")
	require.ErrorContains(t, err, "teammate")
	require.Len(t, bodies, 2)
}

func TestExplicitTeammateHealthFailuresDoNotDegrade(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_TEAMMATE", "explicit-worker")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi"}, harness.Detected)
	for _, failure := range []string{"auth", "invalid JSON", "transport"} {
		t.Run(failure, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes.Add(1)
				}
				switch failure {
				case "auth":
					w.WriteHeader(http.StatusUnauthorized)
				case "invalid JSON":
					_, _ = w.Write([]byte("{broken"))
				case "transport":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}
			}))
			t.Cleanup(server.Close)
			_, stderr, err := executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL), "--project", "example-project", "comment", "abc1", "--body", "progress")
			require.Error(t, err)
			require.Zero(t, writes.Load())
			require.NotContains(t, stderr, "writing without it")
		})
	}
}

func TestCommentSucceedsWhenAncestryErrors(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	// A failed ancestry probe cannot disambiguate nested harness markers.
	for _, key := range harness.EnvKeys() {
		unsetTeammateEnv(t, key)
	}
	t.Setenv("CODEX_THREAD_ID", "example-session")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "00000000-0000-0000-0000-000000000001")
	previous := detectTeammateHarness
	t.Cleanup(func() { detectTeammateHarness = previous })
	detectTeammateHarness = func() (harness.Detection, harness.Outcome) {
		return harness.Detect(os.LookupEnv, unavailableTeammateAncestry{}, "")
	}
	ref := createIssue(t, env, pid, "Existing issue")
	runCLI(t, env, dir, "comment", ref, "--body", "progress")
	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	require.Empty(t, got.Comments[0].Teammate)
}

func TestTeammateInferredCreatePreservesExplicitMetadata(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi"}, harness.Detected)
	var metadata map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.Equal(t, "pi", r.Header.Get("X-Kata-Teammate"))
		if strings.HasSuffix(r.URL.Path, "/issues") {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			metadata, _ = body["metadata"].(map[string]any)
			_, _ = w.Write([]byte(`{"issue":{"id":1,"project_id":1,"short_id":"abc1","title":"New issue","revision":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct{ name, handle string }{{"absent", ""}, {"explicit", "explicit-worker"}, {"empty", ""}} {
		args := []string{"--project", "example-project", "create", "New issue " + tc.name, "--force-new"}
		if tc.name != "absent" {
			args = append(args, "--meta", "teammate="+tc.handle)
		}
		_, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), args...)
		require.NoError(t, err)
		if tc.name == "absent" {
			require.NotContains(t, metadata, "teammate")
		} else {
			require.Equal(t, tc.handle, metadata["teammate"])
		}
	}
}

func TestTeammateNotifyAndInboxKeepExplicitAddressing(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_INBOX_USER", "")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi", Model: "example"}, harness.Detected)
	ref := createIssue(t, env, pid, "Existing issue")
	for _, mode := range []string{"model", "off"} {
		t.Setenv("KATA_TEAMMATE_INFER", mode)
		runCLI(t, env, dir, "--as", "coordinator", "notify", ref, "--to", "reviewer", "--message", "review")
		var inbox inboxOutput
		require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "inbox", "--for", "reviewer", "--json")), &inbox))
		require.Len(t, inbox.Requests, 1)
		require.Equal(t, "coordinator", inbox.Requests[0].From)
		require.Empty(t, inbox.Requests[0].Teammate)
		_, err := runCLICapture(t, env, dir, "inbox")
		require.ErrorContains(t, err, "inbox recipient is required")
	}
}

func TestTeammateWaitIgnoresInferredInbox(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_INBOX_USER", "")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi"}, harness.Detected)
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
		case "/api/v1/instance":
			_, _ = w.Write([]byte(`{"auth":{}}`))
		default:
			message := "before"
			if polls.Add(1) >= 2 {
				message = "after"
			}
			_, _ = fmt.Fprintf(w, `{"issue":{"short_id":"abcd","status":"open","metadata":{%q:{"message":%q},%q:{"message":"stable"}}}}`, notification.MetadataKey("reader"), message, notification.MetadataKey("reader/pi"))
		}
	}))
	t.Cleanup(server.Close)
	stdout, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), "--as", "reader", "--json", "wait", "example-project#abcd", "--until", "reply", "--timeout", "500ms", "--poll-interval", "10ms")
	require.NoError(t, err)
	result := parseWaitJSON(t, stdout)
	require.Equal(t, "reply", result.Results[0].Reason)
}

func TestTeammateShowInboundUsesOnlyExplicitHandle(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "pi"}, harness.Detected)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Inbound replies", Author: "reader"})
	require.NoError(t, err)
	for _, handle := range []string{"", "explicit-worker", "pi"} {
		root, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reader", Teammate: handle, Body: "root " + handle})
		require.NoError(t, err)
		_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "responder", Body: "response " + cmp.Or(handle, "actor"), ReplyToUID: root.UID, ReplyKind: "reply"})
		require.NoError(t, err)
	}
	out := runCLI(t, env, dir, "--as", "reader", "--json", "show", issue.ShortID, "--inbound")
	require.Contains(t, out, "response actor")
	// An actor-only selector intentionally includes every teammate of that actor.
	require.Contains(t, out, "response pi")
	require.Contains(t, out, "response explicit-worker")
	t.Setenv("KATA_TEAMMATE_INFER", "off")
	require.Equal(t, out, runCLI(t, env, dir, "--as", "reader", "--json", "show", issue.ShortID, "--inbound"))
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	out = runCLI(t, env, dir, "--as", "reader", "--teammate", "explicit-worker", "--json", "show", issue.ShortID, "--inbound")
	require.Contains(t, out, "response explicit-worker")
	require.NotContains(t, out, "response actor")
	require.NotContains(t, out, "response pi")
}

func TestDoctorTeammateInferenceErrorStillProducesReport(t *testing.T) {
	home, workspace := doctorTestEnv(t)
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	unsetTeammateEnv(t, "KATA_TEAMMATE_INFER")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[teammate]\ninfer='maybe'"), 0600))
	stdout, _, err := executeRootCapture(t, t.Context(), "--workspace", workspace, "doctor", "--json")
	require.ErrorContains(t, err, "doctor found failed checks")
	var report diagnostics.Report
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	require.Equal(t, diagnostics.StatusFail, doctorFinding(t, report, "config.daemon").Status)
}

// The ancestry interface represents an unavailable process probe with ok=false.
type unavailableTeammateAncestry struct{}

func (unavailableTeammateAncestry) Nearest([]string) (string, string, bool) {
	return "", "", false
}

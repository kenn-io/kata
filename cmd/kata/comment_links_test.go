package main

import (
	"encoding/hex"
	"encoding/json/v2"
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

func checkCommentIdempotencyHeader(t *testing.T, data []byte) {
	t.Helper()
	t.Setenv("KATA_TEAMMATE", "")
	key := hex.EncodeToString(data[:min(len(data), 8)])
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Finding")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	seen := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			seen <- append([]string(nil), r.Header.Values("Idempotency-Key")...)
		}
		r.Host = target.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	proxied := *env
	proxied.URL = server.URL
	args := []string{"comment", ref, "--body", "Evidence"}
	if key != "" {
		args = append(args, "--idempotency-key", key)
	}
	runCLIAs(t, &proxied, dir, "example-agent", args...)
	values := <-seen
	if key == "" {
		require.Empty(t, values, "plain comments must omit the optional idempotency header")
	} else {
		require.Equal(t, []string{key}, values)
	}
}

func TestPlainCommentOmitsEmptyIdempotencyHeader(t *testing.T) {
	checkCommentIdempotencyHeader(t, nil)
}

func FuzzCommentIdempotencyHeader(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 2})
	f.Fuzz(checkCommentIdempotencyHeader)
}

func TestPartialBacklinkEvidenceIsVisibleInHumanAndAgentOutput(t *testing.T) {
	var response showResponseForCLI
	require.NoError(t, json.Unmarshal([]byte(`{"issue":{"uid":"finding"},"comments":[{"uid":"comment","body":"Finding","backlinks_truncated":true}]}`), &response))
	var human strings.Builder
	require.NoError(t, printCommentAnnotations(&human, response.Comments[0]))
	require.Contains(t, human.String(), "More replies may be available")
	var agent strings.Builder
	require.NoError(t, printShowAgent(&agent, response, "example-project", "show"))
	require.Contains(t, agent.String(), "backlinks_truncated=true")
}

func TestCommentTypedReplyCLI(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "Finding")
	runCLI(t, env, dir, "comment", ref, "-m", "Finding")
	shown := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	target := shown.Comments[0].UID
	handle := "c:" + strings.ToLower(target[len(target)-6:])
	runCLI(t, env, dir, "comment", ref, "--reply", handle, "--idempotency-key", "reply-key", "-m", "Answer")
	runCLI(t, env, dir, "comment", ref, "--reply", target, "--idempotency-key", "reply-key", "-m", "Answer")
	require.Len(t, fetchIssueViaHTTPWithComments(t, env, pid, ref).Comments, 2)
	human := runCLI(t, env, dir, "show", ref, "--thread", handle, "--kind", "reply")
	require.Contains(t, human, "↳ reply "+handle)
	require.Contains(t, human, "← reply")
	agent := runCLI(t, env, dir, "--agent", "show", ref, "--inbound", "--kind", "reply")
	require.Contains(t, agent, "reply")
	require.Contains(t, agent, target)
}

func TestShowInboundDefaultsToAuthenticatedActor(t *testing.T) {
	checkShowInboundActor(t, "daemon-operator", "workspace-agent")
}

func FuzzShowInboundDefaultsToAuthenticatedActor(f *testing.F) {
	f.Add("operator")
	f.Fuzz(func(t *testing.T, seed string) {
		if len(seed) > 16 {
			seed = seed[:16]
		}
		authActor := "daemon-" + hex.EncodeToString([]byte(seed))
		checkShowInboundActor(t, authActor, "workspace-agent")
	})
}

func checkShowInboundActor(t *testing.T, authActor, workspaceActor string) {
	t.Helper()
	env, dir, pid := setupCLIWorkspaceOptions(t,
		testenv.WithAuthToken("bootstrap-token"),
		testenv.WithRequireTokenIdentity(),
	)
	const operatorToken = "operator-bearer"
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{ //nolint:gosec // test-only bearer credential
		PlaintextToken: operatorToken,
		Actor:          authActor,
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	t.Setenv("KATA_AUTH_TOKEN", operatorToken)
	t.Setenv("KATA_AUTHOR", workspaceActor)
	t.Setenv("KATA_TEAMMATE", "")
	t.Setenv("KATA_INBOX_USER", "")

	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: pid, Title: "Inbound actor", Author: "issue-author",
	})
	require.NoError(t, err)
	operatorRoot, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: authActor, Body: "operator target",
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "responder", Body: "operator reply",
		ReplyToUID: operatorRoot.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	workspaceRoot, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: workspaceActor, Body: "workspace target",
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "responder", Body: "workspace reply",
		ReplyToUID: workspaceRoot.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)

	out := runCLI(t, env, dir, "--json", "show", issue.ShortID, "--inbound")
	require.Contains(t, out, "operator reply")
	require.NotContains(t, out, "workspace reply")

	explicit := runCLI(t, env, dir, "--json", "show", issue.ShortID, "--inbound=workspace-agent")
	require.Contains(t, explicit, "workspace reply")
	require.NotContains(t, explicit, "operator reply")
}

func TestCommentTypedFlagsMutuallyExclusive(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	_, stderr, err := executeRootCapture(t, t.Context(), "comment", "abc4", "--reply", "c:abc123", "--refute", "c:abc123", "-m", "body")
	require.Error(t, err)
	require.Contains(t, stderr, "reply")
	require.Contains(t, stderr, "refute")
}

func TestAgentCrossIssueThreadHandle(t *testing.T) {
	b := showResponseForCLI{}
	b.Issue.UID = "root-issue"
	b.Comments = []cliShowComment{{UID: "01BBBBBBBBBBBBBBBBBBBBBBBB", IssueUID: "other-issue", IssueShortID: "abcd", Handle: "c:bbbbbb", Author: "worker", Body: "Answer"}}
	var out strings.Builder
	require.NoError(t, printShowAgent(&out, b, "example-project", "show"))
	require.Contains(t, out.String(), "abcd:bbbbbb", "thread comment's advertised handle must resolve from shown issue")
}

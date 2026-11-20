package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestLabelAdd_HappyPath(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	out := runCLI(t, env, dir, "label", "add", ref, "needs-review")
	assert.Contains(t, out, "needs-review")
}

func TestLabelAdd_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	out := runCLI(t, env, dir, "--agent", "label", "add", ref, "needs-review")

	assert.Regexp(t, `(?m)^OK label \S+ changed=true`, out)
	assert.Contains(t, out, "Label: needs-review")
	assert.Contains(t, out, "Action: added")
}

func TestLabelRm_HappyPath(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "bug")
	out := runCLI(t, env, dir, "label", "rm", ref, "bug")
	assert.True(t, strings.Contains(out, "removed") || strings.Contains(out, "unlabeled"))
}

func TestLabelRm_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")
	runCLI(t, env, dir, "label", "add", ref, "bug")

	resetFlags(t)
	out := runCLI(t, env, dir, "--agent", "label", "rm", ref, "bug")

	assert.Regexp(t, `(?m)^OK label \S+ changed=true`, out)
	assert.Contains(t, out, "Label: bug")
	assert.Contains(t, out, "Action: removed")
}

func TestLabelsList_PrintsCounts(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "bug")
	out := runCLI(t, env, dir, "labels")
	assert.Contains(t, out, "bug")
	assert.Contains(t, out, "1")
}

func TestLabelsList_AgentOutputIncludesCount(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "bug")
	runCLI(t, env, dir, "label", "add", ref, "safari")
	out := runCLI(t, env, dir, "--agent", "labels")

	assert.Contains(t, out, "OK labels count=2\n")
	assert.Contains(t, out, "- label=bug count=1\n")
	assert.Contains(t, out, "- label=safari count=1")
}

// TestLabel_RejectsEmptyLabel covers hammer-test finding #8: label rm with an
// empty value used to URL-encode to "" and hit /labels/?actor=... which the
// daemon answered with a raw 404 page. Now both add and rm reject
// client-side with a uniform validation message.
func TestLabel_RejectsEmptyLabel(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")
	for _, args := range [][]string{
		{"label", "add", ref, ""},
		{"label", "rm", ref, "  "},
	} {
		resetFlags(t)
		_, err := runCLICapture(t, env, dir, args...)
		require.Errorf(t, err, "args %v should reject", args)
		ce := requireCLIError(t, err, ExitValidation)
		assert.Contains(t, ce.Message, "label must not be empty")
	}
}

func TestLabelAdd_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "flaky", "--comment", "intermittent in CI")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "intermittent in CI", got.Comments[0].Body)
}

func TestLabelRm_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "a")
	runCLI(t, env, dir, "label", "add", ref, "flaky")

	runCLI(t, env, dir, "label", "rm", ref, "flaky", "--comment", "stable since 4.6")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "stable since 4.6", got.Comments[0].Body)
}

// TestCreate_RejectsWhitespaceLabel covers the create --label case from
// hammer #8. Pflag's StringSliceVar drops a literal empty argument (""),
// but a whitespace-only label like "   " makes it through and used to be
// silently dropped by the daemon. Reject client-side instead.
func TestCreate_RejectsWhitespaceLabel(t *testing.T) {
	resetFlags(t)
	_, _, err := executeRootCapture(t, t.Context(), "create", "title", "--label", "   ")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "label must not be empty")
}

func issueLabelsViaShow(t *testing.T, env *testenv.Env, dir, ref string) []string {
	t.Helper()
	var shown struct {
		Issue struct {
			Labels []string `json:"labels"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(runCLI(t, env, dir, "--json", "show", ref)), &shown))
	return shown.Issue.Labels
}

func TestLabelAdd_RemoveSwapsLabelsAndPrintsResult(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")
	runCLI(t, env, dir, "label", "add", ref, "ready-for-agent")
	runCLI(t, env, dir, "label", "add", ref, "keep")

	out := runCLI(t, env, dir, "label", "add", ref, "ready-for-human", "--remove", "ready-for-agent")

	assert.Contains(t, out, "keep, ready-for-human")
	assert.NotContains(t, out, "ready-for-agent")
	assert.ElementsMatch(t, []string{"keep", "ready-for-human"}, issueLabelsViaShow(t, env, dir, ref))
}

func TestLabelAdd_RemoveAgentAndJSONOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")
	runCLI(t, env, dir, "label", "add", ref, "ready-for-agent")

	agent := runCLI(t, env, dir, "--agent", "label", "add", ref, "ready-for-human", "--remove", "ready-for-agent")
	assert.Regexp(t, `(?m)^OK label \S+ changed=true`, agent)
	assert.Contains(t, agent, "Labels: ready-for-human")

	jsonOut := runCLI(t, env, dir, "--json", "label", "add", ref, "x-new", "--remove", "ready-for-human")
	var got struct {
		Labels []string `json:"labels"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &got))
	assert.Equal(t, []string{"x-new"}, got.Labels)
}

func TestLabelAdd_RemoveRefusesOldDaemonWithoutWriting(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")
	runCLI(t, env, dir, "label", "add", ref, "ready-for-agent")

	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.61.0"}`))
			return
		}
		if r.Method != http.MethodGet && r.URL.Path != "/api/v1/projects/resolve" {
			writes.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	old := *env
	old.URL = server.URL

	_, err = runCLICapture(t, &old, dir, "label", "add", ref, "ready-for-human", "--remove", "ready-for-agent")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires daemon API 0.70.0")
	assert.Zero(t, writes.Load())
	assert.Equal(t, []string{"ready-for-agent"}, issueLabelsViaShow(t, env, dir, ref))
}

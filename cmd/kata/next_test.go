package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func createNextTestIssue(t *testing.T, env *testenv.Env, projectID int64, title string, priority *int64) string {
	t.Helper()
	payload := map[string]any{"actor": "tester", "title": title}
	if priority != nil {
		payload["priority"] = *priority
	}
	type response struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	created := postJSON[response](t, env.URL+"/api/v1/projects/"+itoa(projectID)+"/issues", payload)
	return created.Issue.ShortID
}

func createFullNextTestIssue(t *testing.T, env *testenv.Env, dir, title string) string {
	t.Helper()
	ref := trimLine(runCLI(t, env, dir, "--as", "tester", "--quiet", "create", title,
		"--body", "Detailed issue context.",
		"--priority", "1",
		"--label", "bug",
		"--label", "ready",
		"--meta", "work.mode=full"))
	runCLI(t, env, dir, "--as", "tester", "comment", ref, "--body", "Detailed comment record.")
	return ref
}

func TestNext_HumanSelectsP0AndPrintsOneReadyRow(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	p0 := int64(0)
	highest := createNextTestIssue(t, env, pid, "highest priority", &p0)
	createNextTestIssue(t, env, pid, "newer unprioritized", nil)

	out := runCLI(t, env, dir, "next")

	assert.Len(t, strings.Split(out, "\n"), 1, "next prints exactly one row")
	assert.Contains(t, out, highest)
	assert.Contains(t, out, "• P0")
	assert.Contains(t, out, "highest priority")
	assert.NotContains(t, out, "newer unprioritized")
	assert.NotContains(t, out, "Ready:")
}

func TestNext_AgentOutputIsOneLine(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	p0 := int64(0)
	ref := createNextTestIssue(t, env, pid, "highest priority", &p0)

	out := runCLI(t, env, dir, "--agent", "next")

	assert.Equal(t, `OK next issue=`+ref+` priority=0 title="highest priority"`, out)
	assert.NotContains(t, out, "owner=")
}

func TestNext_JSONWrapsSelectedRawIssue(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createNextTestIssue(t, env, pid, "json candidate", nil)

	out := runCLI(t, env, dir, "--json", "next")
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Contains(t, got, "issue")
	assert.NotContains(t, got, "issues")
	var issue map[string]any
	require.NoError(t, json.Unmarshal(got["issue"], &issue))
	assert.Equal(t, ref, issue["short_id"])
	assert.Equal(t, "json candidate", issue["title"])
}

func TestNext_EmptyOutputs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "human", args: []string{"next"}, want: "No ready issues."},
		{name: "agent", args: []string{"--agent", "next"}, want: "OK next found=false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, dir, _ := setupCLIWorkspace(t)
			assert.Equal(t, tt.want, runCLI(t, env, dir, tt.args...))
		})
	}

	t.Run("json", func(t *testing.T) {
		env, dir, _ := setupCLIWorkspace(t)
		out := runCLI(t, env, dir, "--json", "next")
		var got map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		assert.Equal(t, "null", string(got["issue"]))
		assert.NotContains(t, got, "issues")
	})
}

func TestNext_HumanFullUsesShowDetail(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	ref := createFullNextTestIssue(t, env, dir, "full detail candidate")

	out := runCLI(t, env, dir, "next", "--full")

	assert.Contains(t, out, ref+"  full detail candidate  [open]  by tester")
	assert.Contains(t, out, "Detailed issue context.")
	assert.Contains(t, out, "--- comments ---")
	assert.Contains(t, out, "Detailed comment record.")
	assert.Contains(t, out, "--- labels ---")
	assert.Contains(t, out, "bug, ready")
	assert.Contains(t, out, "--- metadata ---")
	assert.Contains(t, out, `work.mode = "full"`)
	assert.NotContains(t, out, "• P1")
}

func TestNext_AgentFullUsesNextHeaderAndShowSections(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	ref := createFullNextTestIssue(t, env, dir, "agent full candidate")

	out := runCLI(t, env, dir, "--agent", "next", "--full")

	assert.True(t, strings.HasPrefix(out, "OK next "+ref+"\n"), out)
	assert.Contains(t, out, "Issue: "+ref+` "agent full candidate"`)
	assert.Contains(t, out, "Status: open\n")
	assert.Contains(t, out, "Labels: bug,ready\n")
	assert.Contains(t, out, "Priority: 1\n")
	assert.Contains(t, out, "Body:\n```text\nDetailed issue context.\n```\n")
	assert.Contains(t, out, "Comments:\n")
	assert.Regexp(t, regexp.MustCompile(`(?m)^- uid=[0-9A-HJKMNP-TV-Z]{26} author=tester created_at=[^ \n]+$`), out)
	assert.Contains(t, out, "Detailed comment record.")
}

func TestNext_JSONFullUsesShowEnvelope(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	ref := createFullNextTestIssue(t, env, dir, "json full candidate")

	out := runCLI(t, env, dir, "--json", "next", "--full")
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &got))

	for _, key := range []string{"issue", "comments", "labels", "links"} {
		assert.Contains(t, got, key)
	}
	assert.NotContains(t, got, "issues")
	var issue map[string]any
	require.NoError(t, json.Unmarshal(got["issue"], &issue))
	assert.Equal(t, ref, issue["short_id"])
	assert.Equal(t, "json full candidate", issue["title"])
}

func TestNext_AllFullResolvesQualifiedProject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	peer := createIssue(t, env, pid, "local peer")
	project, err := env.DB.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	target := createNextTestIssue(t, env, project.ID, "global full candidate", ptrInt64(0))
	postJSONOK(t, env.URL+"/api/v1/projects/"+itoa(project.ID)+"/issues/"+target+"/links",
		map[string]any{"actor": "tester", "type": "related", "to_ref": "kata#" + peer})

	out := runCLI(t, env, dir, "next", "--all", "--full")

	assert.Contains(t, out, target+"  global full candidate  [open]  by tester")
	assert.Contains(t, out, "related: kata#"+peer)
}

func TestNext_EmptyFullMatchesCompactOutputs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "human", args: []string{"next"}},
		{name: "agent", args: []string{"--agent", "next"}},
		{name: "json", args: []string{"--json", "next"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, dir, _ := setupCLIWorkspace(t)
			compact := runCLI(t, env, dir, tt.args...)
			fullArgs := append(append([]string(nil), tt.args...), "--full")
			assert.Equal(t, compact, runCLI(t, env, dir, fullArgs...))
		})
	}
}

func TestNext_OwnershipAndLabelFiltersMirrorReady(t *testing.T) {
	t.Run("ownership", func(t *testing.T) {
		env, dir, pid := setupCLIWorkspace(t)
		unowned := createIssue(t, env, pid, "unowned candidate")
		owned := createIssue(t, env, pid, "owned candidate")
		runCLI(t, env, dir, "assign", owned, "alice")

		assert.Contains(t, runCLI(t, env, dir, "next", "--unowned"), unowned)
		assert.Contains(t, runCLI(t, env, dir, "next", "--owner", "alice"), owned)
	})

	t.Run("labels", func(t *testing.T) {
		env, dir, pid := setupCLIWorkspace(t)
		plain := createIssue(t, env, pid, "plain candidate")
		bug := createIssue(t, env, pid, "bug candidate")
		runCLI(t, env, dir, "label", "add", bug, "bug")

		assert.Contains(t, runCLI(t, env, dir, "next", "--label", "bug"), bug)
		assert.Contains(t, runCLI(t, env, dir, "next", "--no-label", "bug"), plain)
	})
}

func TestNext_AllQualifiesRefAndPreservesProjectNameInJSON(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "global candidate")

	agentOut := runCLI(t, env, dir, "--agent", "next", "--all")
	assert.Contains(t, agentOut, "issue=kata#"+ref)

	jsonOut := runCLI(t, env, dir, "--json", "next", "--all")
	var got struct {
		Issue map[string]any `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &got))
	assert.Equal(t, "kata", got.Issue["project_name"])
}

func TestNext_AllValidationMirrorsReady(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "project", args: []string{"--project", "anything", "next", "--all"}, want: "mutually exclusive"},
		{name: "unowned", args: []string{"next", "--all", "--unowned"}, want: "--all does not support"},
		{name: "owner", args: []string{"next", "--all", "--owner", "alice"}, want: "--all does not support"},
		{name: "label", args: []string{"next", "--all", "--label", "bug"}, want: "--all does not support"},
		{name: "no-label", args: []string{"next", "--all", "--no-label", "wip"}, want: "--all does not support"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, dir, _ := setupCLIWorkspace(t)
			_, err := runCLICapture(t, env, dir, tt.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNext_UnownedAndOwnerAreMutuallyExclusive(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	_, err := runCLICapture(t, env, dir, "next", "--unowned", "--owner", "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestNext_HasNoLimitFlag(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	_, err := runCLICapture(t, env, dir, "next", "--limit", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag: --limit")
}

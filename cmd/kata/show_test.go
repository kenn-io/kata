package main

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShow_RendersLabelsAndLinksSections(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	parent := createIssue(t, env, pid, "parent")
	child := createIssue(t, env, pid, "child")
	// Two labels so we exercise the comma-join.
	for _, label := range []string{"bug", "priority:high"} {
		runCLI(t, env, dir, "label", "add", child, label)
	}
	createLinkViaHTTP(t, env, pid, child, "parent", parent)

	out := runCLI(t, env, dir, "show", child)
	// Exact section headers and comma-joined label rendering.
	assert.Contains(t, out, "--- labels ---")
	assert.Contains(t, out, "bug, priority:high")
	// Links section: viewer (child) is on the "from" side of (from=child parent to=parent)
	// so it reads "parent: <parent_short_id>" — its parent is the parent issue.
	assert.Contains(t, out, "--- links ---")
	assert.Contains(t, out, "parent: "+parent)
}

func TestShow_AgentOutputRendersIssueBodyLabelsAndComments(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	type createResp struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	created := postJSON[createResp](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		map[string]any{
			"actor":    "tester",
			"title":    "Safari callback",
			"body":     "Safari can double-submit the callback.",
			"priority": int64(2),
			"labels":   []string{"bug", "safari"},
		})
	runCLI(t, env, dir, "--as", "tester", "comment", created.Issue.ShortID, "--body", "Reproduced on macOS.")

	out := runCLI(t, env, dir, "--agent", "show", created.Issue.ShortID)

	assert.Contains(t, out, "OK show "+created.Issue.ShortID+"\n")
	assert.Contains(t, out, "Issue: "+created.Issue.ShortID+" \"Safari callback\"\n")
	assert.Contains(t, out, "Status: open\n")
	assert.Contains(t, out, "Labels: bug,safari\n")
	assert.Contains(t, out, "Priority: 2\n")
	assert.Contains(t, out, "Body:\n```text\nSafari can double-submit the callback.\n```\n")
	assert.Regexp(t, regexp.MustCompile(`(?m)^- author=tester created_at=[^ \n]+$`), out)
	assert.Contains(t, out, "\n```text\nReproduced on macOS.\n```")
	assert.NotContains(t, out, "Owner:")
}

func TestShow_AgentOutputLinkRowsUseExistingLinkResponseFields(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	blocker := createIssue(t, env, pid, "blocker")
	blocked := createIssue(t, env, pid, "blocked title")
	createLinkViaHTTP(t, env, pid, blocker, "blocks", blocked)

	out := runCLI(t, env, dir, "--agent", "show", blocker)

	assert.Contains(t, out, "Links:\n")
	assert.Contains(t, out, "- type=blocks issue="+blocked)
	assert.NotContains(t, out, `title="blocked title"`)
}

func TestShow_AgentOutputLinkRowsUsePOVLabels(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	blocker := createIssue(t, env, pid, "blocker")
	blocked := createIssue(t, env, pid, "blocked")
	createLinkViaHTTP(t, env, pid, blocker, "blocks", blocked)

	out := runCLI(t, env, dir, "--agent", "show", blocked)

	assert.Contains(t, out, "- type=blocked-by issue="+blocker)
	assert.NotContains(t, out, "- type=blocks issue="+blocker)
}

// TestShow_LinkLabelInvertsOnToSide verifies that when show runs against
// the link's "to" side, the rendered LABEL inverts to read from the
// viewer's perspective: the parent slot's "to" end is the parent of
// the "from" end, so from the parent's POV (parent of child), the link
// reads "child: <child_short_id>".
func TestShow_LinkLabelInvertsOnToSide(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	parent := createIssue(t, env, pid, "parent")
	child := createIssue(t, env, pid, "child")
	// child → parent stores (from=child, to=parent). Showing parent puts
	// us on the to side.
	createLinkViaHTTP(t, env, pid, child, "parent", parent)

	out := runCLI(t, env, dir, "show", parent)
	assert.Contains(t, out, "child: "+child,
		"showing the parent issue must label the link as `child` from its POV")
}

func TestShow_AcceptsBareUIDAndQualified(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	created := createIssueViaHTTPFull(t, env, dir, "uid target")
	_ = pid // pid not needed; we resolve via created.ShortID

	for _, ref := range []string{created.ShortID, "kata#" + created.ShortID, created.UID} {
		out := runCLI(t, env, dir, "show", ref)
		assert.Contains(t, out, "uid target", "ref %s", ref)
	}
}

// TestShow_LegacyNumberFails pins that bare numeric refs no longer resolve.
// The ResolveRef helper rejects them up-front with a guidance message.
func TestShow_LegacyNumberFails(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	_, err := runCLICapture(t, env, dir, "show", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy issue number")
}

// TestShow_BareRefHonorsProjectFlagOutsideWorkspace pins that --project
// is consulted by ResolveRef as the fallback project name for bare
// refs when the workspace has no .kata.toml binding. Earlier the code
// passed only workspaceProjectName(start) so --project was ignored and
// the user got "no project bound to this workspace" even though they
// had explicitly named one.
func TestShow_BareRefHonorsProjectFlagOutsideWorkspace(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	created := createIssueViaHTTPFull(t, env, dir, "ref outside workspace")

	// Use a fresh temp dir with no .kata.toml so workspaceProjectName
	// returns "". The --project flag must supply the project binding
	// ResolveRef needs to resolve the bare short_id.
	outside := t.TempDir()
	out := runCLI(t, env, outside, "--project", "kata", "show", created.ShortID)
	assert.Contains(t, out, "ref outside workspace")
}

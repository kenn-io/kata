package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuickstart_PrintsAgentInstructions(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newQuickstartCmd()))
	assert.Contains(t, out, "kata agent quickstart")
	assert.Contains(t, out, "Search before creating")
	assert.Contains(t, out, "Do not run delete or purge")
	assert.Contains(t, out, "Default to --agent for ordinary kata reads and mutations in agent logs.")
	assert.Contains(t, out, "Use --json only when your script needs complete structured data")
	assert.Contains(t, out, `kata search "login race" --agent`)
	assert.Contains(t, out, `kata events --after 0 --limit 100 --agent`)
}

func TestQuickstart_PromotesCloseStep(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newQuickstartCmd()))
	idx := strings.Index(out, "kata close")
	require.GreaterOrEqual(t, idx, 0, "quickstart must mention kata close")
	require.LessOrEqual(t, idx, 800,
		"close discipline should appear early in the quickstart")
	assert.Contains(t, out, "asserts that the work is complete")
	assert.Contains(t, out, "--evidence")
	assert.Contains(t, out, "needs-review")
}

func TestQuickstart_JSON(t *testing.T) {
	resetFlags(t)
	out := executeRoot(t, newRootCmd(), "--json", "quickstart")
	var got struct {
		APIVersion int    `json:"kata_api_version"`
		Quickstart string `json:"quickstart"`
	}
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, 1, got.APIVersion)
	assert.Contains(t, got.Quickstart, "kata agent quickstart")
	assert.Contains(t, got.Quickstart, "Default to --agent for ordinary kata reads and mutations in agent logs.")
	assert.Contains(t, got.Quickstart, "Use --json only when your script needs complete structured data")
	assert.Contains(t, got.Quickstart, "kata events --after 0 --limit 100 --agent")
}

func TestQuickstart_AgentOutput(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "--agent", "quickstart"))
	assert.Truef(t, strings.HasPrefix(out, "OK quickstart\n"), "got %q", out)
	assert.NotContains(t, out, "# kata agent quickstart")
	assert.NotContains(t, out, "Remote daemon")
	assert.Contains(t, out, "Default to --agent for ordinary kata reads and mutations in agent logs.")
	assert.Contains(t, out, "Use --json only when your script needs complete structured data.")
}

func TestQuickstart_AgentInstructionsAliasMentionsAgentOutput(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "agent-instructions"))
	assert.Contains(t, out, "Default to --agent for ordinary kata reads and mutations in agent logs.")
	assert.Contains(t, out, "Use --json only when your script needs complete structured data")
}

func TestQuickstart_UsesValidNeedsReviewCommand(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newQuickstartCmd()))
	// kata edit has no --label flag; the needs-review hint must use the real
	// kata label add command so agents do not copy an invalid command.
	assert.Contains(t, out, "kata label add <ref> needs-review")
	assert.NotContains(t, out, "--label needs-review")
}

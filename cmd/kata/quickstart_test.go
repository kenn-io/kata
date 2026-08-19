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
	assert.Contains(t, out, "Do not create practice, tutorial, example, or scratchpad issues")
	assert.Contains(t, out, "Do not run delete or purge")
	assert.Contains(t, out, "Default to --agent for ordinary kata reads and mutations in agent logs.")
	assert.Contains(t, out, "Use --json only when your script needs complete structured data")
	assert.Contains(t, out, `kata search "login race" --agent`)
	assert.Contains(t, out, "kata next --unowned --agent")
	assert.Contains(t, out, "kata ready --unowned --label bug --no-label blocked --agent")
	assert.Contains(t, out, `kata events --after 0 --limit 100 --agent`)
}

func TestQuickstart_IncludesScheduleDeadlineAndSomedayCommands(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newQuickstartCmd()))

	assert.Contains(t, out, "kata schedule <ref> <date-or-time>")
	assert.Contains(t, out, "kata deadline <ref> <date-or-time>")
	assert.Contains(t, out, "kata meta set <ref> someday true --json-value")
	assert.Contains(t, out, "kata meta unset <ref> someday")
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
	assert.Contains(t, out, "valid evidence")
	assert.Contains(t, out, "not in a batch")
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
	assert.Contains(t, got.Quickstart, "kata next --unowned --agent")
	assert.Contains(t, got.Quickstart, "kata ready --unowned --label bug --no-label blocked --agent")
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
	assert.Contains(t, out, "Do not create practice, tutorial, example, or scratchpad issues.")
	assert.Contains(t, out, "kata next --unowned --agent")
	assert.Contains(t, out, "kata ready --unowned --label bug --no-label blocked --agent")
	assert.Contains(t, out, "Close each verified issue promptly; valid evidence keeps sibling close bursts admissible by default.")
}

func TestQuickstart_ContractPrintsManagedWorkflowWithoutMarkers(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "quickstart", "--format", "contract"))

	assert.True(t, strings.HasPrefix(out, "Kata is the system of record for intent."))
	assert.Contains(t, out, "Never `kata delete` or `kata purge` without explicit user authorization.")
	assert.Contains(t, out, "~~~dot\ndigraph kata {")
	assert.Contains(t, out, "}\n~~~\n")
	assert.Contains(t, out, "kata meta set <ref> work.attention ok")
	assert.Contains(t, out, "kata meta set <ref> work.branch <branch>")
	assert.Contains(t, out, "kata create ... --meta work.branch=<branch> --idempotency-key <key>")
	assert.Contains(t, out, "kata meta set <ref> work.attention stuck|needs-human|ok")
	assert.Contains(t, out, `kata meta set <ref> work.attention_msg \"<why>\"`)
	assert.Contains(t, out, "kata wait <refs> --until attention --any")
	assert.Contains(t, out, ".issue.short_id")
	assert.Contains(t, out, "kata close <ref> --done")
	assert.Contains(t, out, "kata label add <ref> needs-review")
	assert.Contains(t, out, "kata schedule <ref> <date-or-time>")
	assert.Contains(t, out, "kata meta set <ref> someday true --json-value")
	assert.Contains(t, out, "kata meta unset <ref> someday")
	assert.Contains(t, out, "kata deadline <ref> <date-or-time>")
	assert.NotContains(t, out, "20z0", "a universal contract cannot use a project-scoped issue ref")
	assert.NotContains(t, out, agentsBlockBegin)
	assert.NotContains(t, out, agentsBlockEnd)
	assert.NotContains(t, out, "# kata agent quickstart")
}

// The contract is rendered into other projects' AGENTS.md files, so it must
// describe kata itself rather than any single workspace's local conventions.
func TestQuickstart_ContractOmitsNonKataConcepts(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "quickstart", "--format", "contract"))

	for _, absent := range []string{
		"projects/<slug>", // labeling convention kata does not define
		"waiting.for",     // no such metadata key in kata
		"waiting.since",
		"tickler",
		"friction",
	} {
		assert.NotContains(t, out, absent)
	}
}

func TestQuickstart_ContractMatchesManagedBlockBody(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "--format", "contract", "quickstart"))
	managed := agentsManagedBlock()

	require.True(t, strings.HasPrefix(managed, agentsBlockBegin+"\n"))
	require.True(t, strings.HasSuffix(managed, agentsBlockEnd))
	body := strings.TrimPrefix(managed, agentsBlockBegin+"\n")
	body = strings.TrimSuffix(body, agentsBlockEnd)
	assert.Equal(t, out, body)
}

func TestQuickstart_ContractAliasAndSelectorsPreserveOutput(t *testing.T) {
	resetFlags(t)
	want := string(executeRoot(t, newRootCmd(), "--format", "contract", "quickstart"))

	for name, args := range map[string][]string{
		"alias":              {"agent-instructions", "--format", "contract"},
		"workspace selector": {"--workspace", "/path/that/does/not/exist", "quickstart", "--format", "contract"},
		"project selector":   {"quickstart", "--project", "spoke-project", "--format", "contract"},
	} {
		t.Run(name, func(t *testing.T) {
			resetFlags(t)
			assert.Equal(t, want, string(executeRoot(t, newRootCmd(), args...)))
		})
	}
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

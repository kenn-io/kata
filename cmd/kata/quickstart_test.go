package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuickstart_ExternalAgentOnboardingFormats(t *testing.T) {
	for _, format := range []string{"human", "agent", "json"} {
		t.Run(format, func(t *testing.T) {
			resetFlags(t)
			t.Setenv("KATA_HOME", t.TempDir())
			out := executeRoot(t, newRootCmd(), "quickstart", "--format", format)
			instructions := string(out)
			if format == "json" {
				var payload struct {
					Quickstart string `json:"quickstart"`
				}
				require.NoError(t, json.Unmarshal(out, &payload))
				instructions = payload.Quickstart
			}
			assert.Contains(t, instructions, "kata federation identity")
			assert.Contains(t, instructions, "hub administrator")
			assert.Contains(t, instructions, "Keep the selected daemon pointed at the spoke")
			assert.Contains(t, instructions, "https://katatracker.com/docs/operations/federation/")
			assert.Contains(t, instructions, "https://katatracker.com/docs/guide/semantic-search/")

		})
	}
}

func TestQuickstart_HelpExplainsFormatsAndSessionSetup(t *testing.T) {
	for _, args := range [][]string{
		{"quickstart", "--help"},
		{"agent-instructions", "--help"},
		{"--agent", "quickstart", "--help"},
		{"--json", "quickstart", "--help"},
		{"quickstart", "--format", "contract", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out := string(executeRoot(t, newRootCmd(), args...))
			for _, text := range []string{
				"Default: full instructions for agents using kata",
				"--agent: concise instructions for agent logs",
				"--json: instructions in a JSON response",
				"--format contract: the canonical managed contract",
				"exactly what kata agent-hook contract injects",
				"kata agent-hook install --all",
				"Without hooks, run kata quickstart at session start",
				"Setup guides cover federation enrollment and optional embeddings",
			} {
				assert.Contains(t, out, text)
			}
		})
	}
}

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
	assert.Contains(t, out, "kata notify abc4 --to coordinator/teammate-1")
	assert.Contains(t, out, "kata inbox --for coordinator/teammate-1")
	assert.Contains(t, out, "kata inbox --for coordinator/teammate-1 --all")
	assert.Contains(t, out, "kata notify abc4 --to coordinator/teammate-1 --clear")
}

func TestQuickstart_GuardsOwnershipRelease(t *testing.T) {
	resetFlags(t)
	plain := string(executeRoot(t, newQuickstartCmd()))
	jsonOutput := executeRoot(t, newRootCmd(), "--json", "quickstart")
	var decoded struct {
		Quickstart string `json:"quickstart"`
	}
	require.NoError(t, json.Unmarshal(jsonOutput, &decoded))

	for _, output := range []string{plain, decoded.Quickstart} {
		step := strings.SplitN(output, "6. Find and claim available work", 2)
		require.Len(t, step, 2)
		step6 := strings.SplitN(step[1], "7. Use native planning dates", 2)
		require.Len(t, step6, 2)
		assert.Contains(t, step6[0], "kata unassign <ref> --expect-owner <your-actor>")
		assert.NotContains(t, step6[0], "\n   kata unassign <ref>\n")
		assert.Contains(t, step6[0], "A bare unassign clears the current owner")
		assert.Contains(t, step6[0], "kata status <ref>")
		assert.Contains(t, step6[0], "current owner and your effective actor")
		assert.Contains(t, step6[0], "[open] by <name>")
		assert.Contains(t, step6[0], "author, not the owner")
	}
}

func TestQuickstartExplainsTeammateFlow(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "quickstart"))
	for _, text := range []string{
		"KATA_TEAMMATE=teammate-1",
		"KATA_INBOX_USER=coordinator/teammate-1",
		"metadata.teammate",
		"kata comment abc4 --body \"Checked the retry path\"",
		"kata create \"Check retry behavior\" --parent abc4 --idempotency-key retry-teammate-1",
		"kata --teammate=teammate-2 comment abc4 --body \"Independent review\"",
		"kata --teammate='' comment abc4 --body \"Coordinator summary\"",
		"kata notify abc4 --to coordinator --message \"Please decide\"",
		"kata notify abc4 --to coordinator/teammate-1",
		"kata inbox --for coordinator/teammate-1",
		"kata notify abc4 --to coordinator/teammate-1 --clear",
	} {
		require.Contains(t, out, text)
	}
}

func TestQuickstart_IncludesScheduleDeadlineAndSomedayCommands(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newQuickstartCmd()))

	assert.Contains(t, out, "kata schedule <ref> <date-or-time>")
	assert.Contains(t, out, "kata deadline <ref> <date-or-time>")
	assert.Contains(t, out, "kata meta set <ref> someday true --json-value")
	assert.Contains(t, out, "kata meta unset <ref> someday")
	assert.Contains(t, out, "Reached schedules and deadlines use notify.*")
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
	assert.Contains(t, got.Quickstart, "kata notify abc4 --to coordinator/teammate-1")
	assert.Contains(t, got.Quickstart, "kata inbox --for coordinator/teammate-1")
	assert.Contains(t, got.Quickstart, "kata notify abc4 --to coordinator/teammate-1 --clear")
	assert.Contains(t, got.Quickstart, "KATA_TEAMMATE=teammate-1")
	assert.Contains(t, got.Quickstart, "KATA_INBOX_USER=coordinator/teammate-1")
	assert.Contains(t, got.Quickstart, "metadata.teammate")
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
	assert.Contains(t, out, "--to <actor>[/<teammate>]")
	assert.Contains(t, out, "kata inbox --for <actor>[/<teammate>]")
	assert.Contains(t, out, "kata inbox --for <actor>[/<teammate>] --all")
	assert.Contains(t, out, "--to <actor>[/<teammate>] --clear")
	assert.Contains(t, out, "KATA_TEAMMATE=teammate-1")
	assert.Contains(t, out, "KATA_INBOX_USER=coordinator/teammate-1")
	assert.Contains(t, out, "metadata.teammate")
}

func TestQuickstart_ContractPrintsManagedWorkflowWithoutMarkers(t *testing.T) {
	resetFlags(t)
	out := string(executeRoot(t, newRootCmd(), "quickstart", "--format", "contract"))

	assert.Equal(t, agentContractText, out, "contract format prints the injected contract without markers or framing")
	// Hooks inject this text into every session in a kata workspace. Keep it
	// to the single-agent core; coordination detail belongs in kata quickstart.
	assert.LessOrEqual(t, len(out), 2000, "keep the per-session contract compact")
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

func TestQuickstartLocalProfileRecoveryGuidance(t *testing.T) {
	// Product contract: agents retain selected routing and diagnose stopped
	// spokes before attempting any initialization or target change.
	for _, args := range [][]string{{"quickstart"}, {"--agent", "quickstart"}, {"--json", "quickstart"}} {
		resetFlags(t)
		out := string(executeRoot(t, newRootCmd(), args...))
		assert.Contains(t, out, "kata daemon diagnose")
		assert.Contains(t, out, "kata daemon recover")
		assert.Contains(t, out, "Never remove a workspace override to repair a stopped daemon")
	}
}

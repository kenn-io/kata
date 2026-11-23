package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		"kata comment abc4 --body-stdin <<-'EOF'\n\tThe retry path reuses the original export ID.\n\tEOF\n",
		"kata create \"Check retry behavior\" --parent abc4 --idempotency-key retry-teammate-1",
		"kata --teammate=teammate-2 comment abc4 --body-stdin <<-'EOF'\n\tA retry after a timeout creates a second export.\n\tEOF\n",
		"kata --teammate='' comment abc4 --body-stdin <<-'EOF'\n\tBoth retry findings trace to the missing export ID check.\n\tEOF\n",
		"kata notify abc4 --to coordinator --message \"Please decide\"",
		"kata notify abc4 --to coordinator/teammate-1",
		"kata inbox --for coordinator/teammate-1",
		"kata notify abc4 --to coordinator/teammate-1 --clear",
	} {
		require.Contains(t, out, text)
	}
	require.NotContains(t, out, "comment abc4 --body \"")
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

// Product contract: every CLI guide says comments carry findings,
// keeps live status in work.attention_msg, corrects a comment by editing it,
// treats an open, unmerged PR as unfinished, and passes comment text through a
// quoted heredoc whose terminator ends stdin.
func TestGuidesSayCommentsCarryFindings(t *testing.T) {
	guides := map[string]string{}
	for _, format := range []string{"human", "agent", "contract"} {
		resetFlags(t)
		out := executeRoot(t, newRootCmd(), "quickstart", "--format", format)
		guides["quickstart "+format] = string(out)
	}
	for name, text := range guides {
		assert.Contains(t, text, "kata meta set <ref> work.attention_msg \"<what remains>\"", name)
		assert.Contains(t, text, "leave the issue open, update work.attention and work.attention_msg", name)
		assert.Contains(t, text, "kata label add <ref> needs-review", name)
		assert.Contains(t, text, "kata comment edit", name)
		assert.Contains(t, text, "not merged", name)
		if name != "quickstart contract" {
			assertClosedBodyStdinHeredoc(t, name, text)
		}
	}
	t.Run("contract heredoc", func(t *testing.T) {
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("bash unavailable")
		}
		output := guides["quickstart contract"]
		start := strings.Index(output, "kata comment <ref> --body-stdin --agent <<-'EOF'\n")
		require.NotEqual(t, -1, start)
		example := output[start:]
		end := strings.Index(example, "\n\tEOF\n")
		require.NotEqual(t, -1, end)
		example = example[:end+len("\n\tEOF\n")]
		example = strings.Replace(example, "<ref>", "abc4", 1)
		example = strings.Replace(example, "<finding>", "The `printf changed` result costs $HOME.", 1)
		script := "kata() { cat; }\n" + example + "printf 'next command\\n'\n"
		cmd := exec.CommandContext(t.Context(), bash, "-c", script) //nolint:gosec // The script contains a controlled example and fixture text.
		got, err := cmd.CombinedOutput()
		require.NoError(t, err, string(got))
		assert.Equal(t, "The `printf changed` result costs $HOME.\nnext command\n", string(got))
	})
	// The retry after a failed mutation comment, and the edit example, carry
	// comment text, so they use the quoted heredoc too.
	assert.Contains(t, guides["quickstart human"], "retry with a quoted heredoc:\n\n   kata comment <ref> --body-stdin <<-'EOF'\n")
	assert.NotContains(t, guides["quickstart human"], "--body ...")
	resetFlags(t)
	commentHelp := string(executeRoot(t, newRootCmd(), "comment", "--help"))
	assert.Contains(t, commentHelp, "findings a later reader needs")
	assert.Contains(t, commentHelp, "work.attention_msg")
	assert.Contains(t, commentHelp, "kata comment edit")
	assert.Contains(t, commentHelp, "unmerged PR is unfinished")
	assert.Contains(t, commentHelp, "needs-review")
	assertClosedBodyStdinHeredoc(t, "comment help", commentHelp)
	guides["comment help"] = commentHelp
	resetFlags(t)
	editHelp := string(executeRoot(t, newRootCmd(), "comment", "edit", "--help"))
	assert.Contains(t, editHelp, "kata comment edit abc4 <comment-uid> --body-stdin --agent <<-'EOF'")
	assert.NotContains(t, editHelp, "-m \"")
	assertClosedBodyStdinHeredoc(t, "comment edit help", editHelp)
	for _, args := range [][]string{{"--help"}, {"close", "--help"}, {"label", "add", "--help"}, {"claim", "--help"}, {"assign", "--help"}} {
		resetFlags(t)
		guides[strings.Join(args, " ")] = string(executeRoot(t, newRootCmd(), args...))
	}
	// The guides teach timed claims and renewal without --if-unowned.
	for _, name := range []string{"quickstart human", "quickstart agent", "quickstart contract", "--help", "claim --help", "assign --help"} {
		assertClaimsCarryTTL(t, name, guides[name])
		if name != "assign --help" {
			assert.Contains(t, guides[name], "without --if-unowned", name)
		}
	}
	for _, path := range []string{"docs/get-started/quickstart.md", "docs/workflows/agents.md", "docs/operations/agent-orchestration.md"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", path))
		require.NoError(t, err)
		assertClaimsCarryTTL(t, path, string(raw))
	}
	assert.NotContains(t, guides["claim --help"], `--comment "starting on the Safari path"`)
	for name, text := range guides {
		assert.NotRegexp(t, `\bprogress\b`, text, name)
		assert.NotContains(t, text, "needs-review --comment", name)
	}
}

// The <<- operator strips tabs, but not spaces, from a heredoc terminator.
func assertClosedBodyStdinHeredoc(t *testing.T, name, text string) {
	t.Helper()
	open, found := false, 0
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "--body-stdin") && strings.HasSuffix(line, "<<-'EOF'") {
			if open {
				t.Errorf("%s: heredoc opened before the previous one closed", name)
			}
			open = true
			found++
		} else if open && strings.TrimLeft(line, "\t") == "EOF" {
			open = false
		}
	}
	if open {
		t.Errorf("%s: heredoc never closed by an EOF line with only tabs before it", name)
	}
	if found == 0 {
		t.Errorf("%s: no --body-stdin <<-'EOF' example", name)
	}
}

var claimCommand = regexp.MustCompile("kata claim [<\\w][^\\n`<#\"]*")

// assertClaimsCarryTTL checks that each matched claim command names --ttl.
func assertClaimsCarryTTL(t *testing.T, name, text string) {
	t.Helper()
	matches := claimCommand.FindAllString(text, -1)
	assert.NotEmpty(t, matches, name)
	for _, match := range matches {
		command := strings.TrimSpace(match)
		if strings.HasSuffix(command, "[flags]") {
			continue // the cobra usage line is a synopsis, not a command
		}
		assert.Contains(t, command, "--ttl", "%s: %q", name, command)
	}
}

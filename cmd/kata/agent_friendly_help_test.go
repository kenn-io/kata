package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentguide"
)

func TestAgentWorkflowHelpFollowsContract(t *testing.T) {
	help := string(executeRoot(t, newRootCmd(), "--help"))
	_, workflow, found := strings.Cut(help, "Agent workflow (add --agent to each command):\n")
	require.True(t, found, "root help must include the agent workflow")
	workflow, _, found = strings.Cut(workflow, "Full agent contract:")
	require.True(t, found, "the workflow must link to the full contract")

	assert.Contains(t, workflow, `kata search "<one or two distinctive words>" --agent`)
	assert.NotContains(t, workflow, "work.attention")
	assert.Contains(t, workflow, "kata create")
	assert.Contains(t, workflow, "kata claim")
	assert.Contains(t, workflow, "kata close")
	assert.Contains(t, workflow, "--commit <sha>")

	commands := regexp.MustCompile(`(?m)^\s*(kata(?: [a-z][a-z-]*)+)\b`).FindAllStringSubmatch(workflow, -1)
	require.NotEmpty(t, commands)
	for _, command := range commands {
		assert.Contains(t, agentguide.Contract, command[1], "workflow command must appear in the short contract")
	}
}

func TestClaimHelpKeepsAttentionInCoordinatorGuidance(t *testing.T) {
	help := string(executeRoot(t, newRootCmd(), "claim", "--help"))
	assert.NotContains(t, help, "work.attention")
	assert.NotContains(t, help, "After claiming, mark the work tracked")
	assert.Contains(t, help, "--ttl releases the claim after 1m-24h.")
	assert.Contains(t, string(executeRoot(t, newRootCmd(), "meta", "--help")), "work.attention")
	assert.Contains(t, string(executeRoot(t, newRootCmd(), "quickstart")), "work.attention")
}

// The team and project-access commands belong to the administration group,
// which the root help renders for agents that rarely need it. Their help text
// and examples are validated with every other command by TestHelpExamplesParse.
func TestAgentFriendlyHelpGroupsAdministrationCommands(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{{"teams"}, {"federation"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("find %v command: %v", path, err)
		}
		if cmd.GroupID != "admin" {
			t.Errorf("%v GroupID = %q, want %q", path, cmd.GroupID, "admin")
		}
	}
	if !root.ContainsGroup("admin") {
		t.Error("root command does not define the admin command group")
	}
	rootHelp := string(executeRoot(t, newRootCmd(), "--help"))
	if !strings.Contains(rootHelp, "Administer (rarely needed by agents):") {
		t.Error("root help does not render the admin command group")
	}
}

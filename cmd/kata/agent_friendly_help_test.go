package main

import (
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

	for _, surface := range []string{workflow, agentguide.Contract} {
		assert.Contains(t, surface, `kata search "<one or two distinctive words>" --agent`)
		assert.Contains(t, surface, `--test "<command>" --agent`)
	}

	// Valid commands can still fall outside the short workflow. Check every
	// advertised command against the contract, including nested command paths.
	commands, err := helpCommandLines(workflow, true)
	require.NoError(t, err)
	require.NotEmpty(t, commands, "workflow must advertise commands")
	for _, command := range commands {
		argv, err := splitHelpWords(command)
		require.NoError(t, err)
		cmd, _, err := newRootCmd().Find(argv[1:])
		require.NoError(t, err)
		assert.Contains(t, agentguide.Contract, cmd.CommandPath(), "workflow command must appear in the short contract")
	}

	// Prose can mention metadata without adding a command.
	assert.NotContains(t, workflow, "work.attention")
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

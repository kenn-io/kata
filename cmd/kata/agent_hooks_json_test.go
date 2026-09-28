package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func TestAgentHookInspectionPreservesJSONNumberSemantics(t *testing.T) {
	parse := func(value string) []agentHookEntry {
		t.Helper()
		entries, err := parseAgentHookEntries(agenthook.AgentCodex, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"command":"kata --source kata-agent-contract-hook","timeout":`+value+`}]}]}}`))
		require.NoError(t, err)
		require.Len(t, entries, 1)
		return entries
	}
	// Full-field comparisons must not round distinct integers or equate a
	// JSON string with a numeric field. The previous UseNumber parser kept both.
	require.False(t, ownedContractMatches(parse("9007199254740992"), parse("9007199254740993")))
	require.False(t, ownedContractMatches(parse("10"), parse(`"10"`)))
	require.True(t, ownedContractMatches(parse("10"), parse("10")))
}

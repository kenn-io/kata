package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentContractHook_EmitsCodexSessionStartContext(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	stdout, stderr, err := executeRootCapture(t, context.Background(),
		"agent-contract-hook", "--source", "kata-agent-contract-hook")
	require.NoError(t, err)
	assert.Empty(t, stderr)

	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope))
	require.Len(t, envelope, 1, "Codex rejects unknown top-level hook response fields")
	var specific struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	}
	require.NoError(t, json.Unmarshal(envelope["hookSpecificOutput"], &specific))
	assert.Equal(t, "SessionStart", specific.HookEventName)
	assert.Equal(t, agentContractText, specific.AdditionalContext)
}

func TestAgentContractHook_IgnoresUnmanagedInvocation(t *testing.T) {
	for name, args := range map[string][]string{
		"missing source": {"agent-contract-hook"},
		"wrong source":   {"agent-contract-hook", "--source", "user-command"},
	} {
		t.Run(name, func(t *testing.T) {
			resetRunEEntered(t)
			resetFlags(t)
			stdout, stderr, err := executeRootCapture(t, context.Background(), args...)
			require.NoError(t, err)
			assert.Empty(t, stdout)
			assert.Empty(t, stderr)
		})
	}
}

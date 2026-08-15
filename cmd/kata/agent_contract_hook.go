package main

import (
	"encoding/json"

	"github.com/spf13/cobra"
)

const agentContractHookSource = "kata-agent-contract-hook"

// newAgentContractHookCmd adapts the canonical plain-text contract to Codex's
// structured SessionStart hook response. It is launcher plumbing installed by
// init --with-codex-hooks, not a user-facing contract format.
func newAgentContractHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "agent-contract-hook",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 || args[0] != "--source" || args[1] != agentContractHookSource {
				return nil
			}
			response := struct {
				HookSpecificOutput struct {
					HookEventName     string `json:"hookEventName"`
					AdditionalContext string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}{}
			response.HookSpecificOutput.HookEventName = "SessionStart"
			response.HookSpecificOutput.AdditionalContext = agentContractText
			return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
		},
	}
}

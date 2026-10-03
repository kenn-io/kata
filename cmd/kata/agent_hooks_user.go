package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"

	"go.kenn.io/kit/agenthook"
)

func contractRegistrationHook(agent agenthook.Agent) agenthook.Hook {
	if agent == agenthook.AgentHermes {
		return agenthook.Hook{Event: agenthook.EventUserPromptSubmit, Timeout: 10 * time.Second}
	}
	hook := agenthook.Hook{Event: agenthook.EventSessionStart, Timeout: 10 * time.Second}
	if agent == agenthook.AgentCodex {
		hook.Matcher = codexContractSessionStartMatcher
	}
	return hook
}

const codexAgentHookTrustNote = "Codex runs new hooks only after you trust them: open Codex and run /hooks."

type agentHookMutation struct {
	Harness    string   `json:"harness"`
	ConfigPath string   `json:"config_path"`
	Changed    bool     `json:"changed"`
	State      string   `json:"state"`
	Reason     string   `json:"reason"`
	Warnings   []string `json:"warnings"`
}

func validateAgentHookConfigFlag(cmd *cobra.Command, config string) error {
	if cmd.Flags().Changed("config") && config == "" {
		return agentHookUsage("--config must not be empty")
	}
	return nil
}

func agentHookUserCompletion(single bool) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
		if single && len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return nativeHookNames(prefix, args), cobra.ShellCompDirectiveNoFileComp
	}
}

func newAgentHooksUninstallCmd() *cobra.Command { return newNativeAgentHooksMutationCmd(true) }

func printAgentHookMutations(cmd *cobra.Command, verb string, results []agentHookMutation) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), struct {
			Action  string              `json:"action"`
			Results []agentHookMutation `json:"results"`
		}{verb, results})
	}
	if flags.Quiet {
		return nil
	}
	for _, result := range results {
		if currentOutputMode() == outputAgent {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "harness=%s config_path=%s state=%s changed=%t reason=%s\n", agentValue(result.Harness), agentValue(result.ConfigPath), result.State, result.Changed, agentValue(result.Reason)); err != nil {
				return err
			}
		} else {
			state := result.State
			if result.Reason != "" {
				state += " (" + result.Reason + ")"
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s — %s\n", result.Harness, state, textsafe.Line(result.ConfigPath)); err != nil {
				return err
			}
		}
		for _, warning := range result.Warnings {
			if currentOutputMode() == outputAgent {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "warning=%s\n", agentValue(warning)); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintln(cmd.OutOrStdout(), textsafe.Line(warning)); err != nil {
				return err
			}
		}
	}
	return nil
}

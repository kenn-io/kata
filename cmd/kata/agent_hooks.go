package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
	"golang.org/x/term"
)

func newAgentHooksCmd() *cobra.Command {
	return newAgentHooksCmdWithTerminalCheck(agentHookInputIsTerminal)
}

func newAgentHooksCmdWithTerminalCheck(isTerminal func(io.Reader) bool) *cobra.Command {
	group := &cobra.Command{
		Use:   "agent-hooks",
		Short: "Manage Kata hooks for coding agents",
		Long: "Manage Kata's contract and attention hooks in coding-agent configurations.\n\n" +
			"These contract and attention entry points are distinct from daemon event\n" +
			"hooks configured in hooks.toml.",
	}
	var source string
	contract := &cobra.Command{
		Use:   "contract <harness>",
		Short: "Read stdin and emit a harness-native contract response",
		Long: "Read a finite native SessionStart JSON payload from stdin through EOF\n" +
			"and emit the kata contract in the harness's native response.\n\n" +
			"Hermes uses pre_llm_call instead, injecting only when extra.is_first_turn is true.\n" +
			"Kit v0.26.0 requires a nonempty text extra.user_message; empty or multimodal messages fail.\n" +
			"Command harnesses: claude, codex, copilot, cursor, gemini, hermes, qwen, droid, antigravity, kimi-code, muse, zcode.\n" +
			"For plain text at a terminal, use kata quickstart --format contract.\n" +
			"--source selects a local UTF-8 prompt file, replacing the entire contract.\n" +
			"Relative paths use cwd; missing files use the built-in contract; empty files supply an empty prompt.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: agentHookHarnessCompletion,
		RunE: func(cmd *cobra.Command, args []string) error {
			capability, err := lookupAgentHookCapability(args[0])
			if err != nil {
				return err
			}
			if capability.Name == "pi" || capability.Name == "openclaw" || capability.Name == "amp" || capability.Name == "opencode" {
				return agentHookUsage(capability.Name + " uses a native extension; install it with kata agent-hooks install " + capability.Name)
			}
			if !capability.Contract {
				return agentHookUsage(capability.Name + " does not consume hook contract context; use committed AGENTS.md")
			}
			text, err := readAgentContractSource(source, cmd.Flags().Changed("source"))
			if err != nil {
				return err
			}
			input := cmd.InOrStdin()
			if isTerminal(input) {
				return agentHookUsage("contract requires a finite JSON payload on stdin; use kata quickstart --format contract for plain text")
			}
			var renderErr error
			if agentHookUsesKit(capability.Name) {
				renderErr = writeNativeAgentContract(cmd.Context(), agenthook.Agent(capability.Name), input, cmd.OutOrStdout(), text)
			} else {
				renderErr = writeExtraAgentContract(capability.Name, input, cmd.OutOrStdout(), text)
			}
			if err := renderErr; err != nil {
				return &cliError{Message: firstLine(err.Error()), Kind: kindInternal, ExitCode: ExitInternal}
			}
			return nil
		},
	}
	contract.Flags().StringVar(&source, "source", "", "local UTF-8 prompt file (missing file uses built-in contract)")
	attention := &cobra.Command{
		Use:   "attention",
		Short: "Track workspace attention at session start and end",
		Long:  "Track work.attention for the workspace issue in KATA_REF.\nThese hooks do not read stdin.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	attention.AddCommand(newAgentHookAttentionCmd("start"), newAgentHookAttentionCmd("end"))
	group.AddCommand(contract, attention, newNativeAgentAttentionCmd(), newNativeAgentHooksMutationCmdWithTerminalCheck(false, isTerminal), newAgentHooksUninstallCmd(), newAgentHooksStatusCmd(), newAgentInstructionsCmd())
	return group
}

func newAgentHookAttentionCmd(mode string) *cobra.Command {
	var source string
	short := "Establish the attention baseline at session start"
	if mode == "end" {
		short = "Raise attention if the session ended without a hand-off"
	}
	cmd := &cobra.Command{
		Use:               mode,
		Short:             short,
		Long:              short + ".\n\nKATA_REF names the tracked workspace issue.\nThe optional --source " + legacyAttentionHookSource + mode + " marker identifies ownership.\nStdin is ignored and daemon failures remain silent.",
		Args:              cobra.NoArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("source") && source != legacyAttentionHookSource+mode {
				return agentHookUsage("--source must be " + legacyAttentionHookSource + mode)
			}
			runAttentionHook(cmd, mode)
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "optional ownership marker: "+legacyAttentionHookSource+mode)
	return cmd
}

func agentHookUsage(message string) error {
	return &cliError{Message: message, Kind: kindUsage, ExitCode: ExitUsage}
}

func parseAgentHookHarness(name string) (agenthook.Agent, error) {
	agent, err := agenthook.ParseAgent(name)
	if err != nil {
		names := make([]string, 0, len(agenthook.Profiles()))
		for _, profile := range agenthook.Profiles() {
			names = append(names, string(profile.Agent))
		}
		return "", agentHookUsage(fmt.Sprintf("unknown harness %q; accepted: %s", name, strings.Join(names, ", ")))
	}
	profile, _ := agenthook.LookupProfile(agent)
	if !slices.Contains(profile.SupportedEvents, agenthook.EventSessionStart) {
		return "", agentHookUsage(profile.DisplayName + " hooks do not support SessionStart")
	}
	return agent, nil
}

func agentHookHarnessCompletion(_ *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	var names []string
	if len(args) == 0 {
		for _, capability := range agentHookCapabilities() {
			if capability.Contract && capability.Name != "pi" && capability.Name != "openclaw" && capability.Name != "amp" && capability.Name != "opencode" && strings.HasPrefix(capability.Name, prefix) {
				names = append(names, capability.Name)
			}
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func agentHookInputIsTerminal(input io.Reader) bool {
	file, ok := input.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

type nativeAgentContractHandler struct {
	agenthook.NoopHandler
	agent         agenthook.Agent
	cursorContext string
	text          string
}

func (h *nativeAgentContractHandler) SessionStart(context.Context, agenthook.SessionStartInput) (agenthook.SessionStartOutput, error) {
	if h.agent == agenthook.AgentHermes {
		return agenthook.SessionStartOutput{}, nil
	}
	if h.agent == agenthook.AgentCursor {
		// Kit v0.26.0 cannot encode Cursor SessionStart context. Return a
		// neutral response for Kit to encode, then bridge the captured context.
		h.cursorContext = h.text
		return agenthook.SessionStartOutput{}, nil
	}
	return agenthook.SessionStartOutput{AdditionalContext: h.text}, nil
}

func (h *nativeAgentContractHandler) UserPromptSubmit(_ context.Context, input agenthook.UserPromptSubmitInput) (agenthook.UserPromptSubmitOutput, error) {
	if h.agent != agenthook.AgentHermes {
		return agenthook.UserPromptSubmitOutput{}, nil
	}
	// Hermes on_session_start ignores responses. Its first pre_llm_call
	// supplies is_first_turn in the native extension preserved by Kit.
	var native struct {
		Extra struct {
			IsFirstTurn jsontext.Value `json:"is_first_turn"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(input.Raw, &native); err != nil {
		return agenthook.UserPromptSubmitOutput{}, err
	}
	if string(native.Extra.IsFirstTurn) != "true" {
		return agenthook.UserPromptSubmitOutput{}, nil
	}
	return agenthook.UserPromptSubmitOutput{AdditionalContext: h.text}, nil
}

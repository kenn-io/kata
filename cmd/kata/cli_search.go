package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

func newCLICmd(root *cobra.Command) *cobra.Command {
	// These terms cover measured misses in the agent-task evaluation. Keep
	// them on the real commands so discovery still follows the live tree.
	for path, keywords := range map[string][]string{
		"comment":       {"log", "note"},
		"create":        {"bug"},
		"show":          {"details"},
		"ready":         {"unblocked"},
		"close":         {"completed"},
		"cron run show": {"recorded"},
		"status":        {"status"},
	} {
		if cmd, rest, err := root.Find(strings.Fields(path)); err == nil && len(rest) == 0 {
			clisearch.SetKeywords(cmd, keywords...)
		}
	}
	cmd := clisearch.NewCommand(root, cliSearchOptions())
	// Keep Kata's flag hints and negative-positional guidance. UsageError
	// still classifies kit's argument validation separately.
	cmd.SetFlagErrorFunc(root.FlagErrorFunc())
	for _, child := range cmd.Commands() {
		child.SetFlagErrorFunc(root.FlagErrorFunc())
	}
	return cmd
}

func cliSearchOptions() clisearch.Options {
	return clisearch.Options{
		Synonyms: map[string][]string{
			"ticket": {"issue"},
			"bug":    {"issue"},
			"todo":   {"issue"},
			"note":   {"comment"},
		},
		Skip: skipCLISearchCommand,
		UsageError: func(err error) error {
			return &cliError{Message: err.Error(), Kind: kindUsage, ExitCode: ExitUsage}
		},
		Render: func(cmd *cobra.Command, response clisearch.Response) error {
			switch currentOutputMode() {
			case outputJSON:
				return emitJSON(cmd.OutOrStdout(), response)
			case outputAgent:
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK search results=%d\n", len(response.Results)); err != nil {
					return err
				}
			}
			return clisearch.RenderText(cmd, response)
		},
	}
}

func skipCLISearchCommand(cmd *cobra.Command) bool {
	// The root is runnable only for --version and help. Exclude just this
	// node; kit still visits its ordinary command children.
	if cmd == cmd.Root() {
		return true
	}
	path := cmd.CommandPath()
	for _, runner := range []string{
		"kata ui", "kata tui", "kata mcp serve", "kata connector serve",
		"kata agent-hook contract", "kata agent-hook attention",
		"kata attention-hook", "kata agent-contract-hook",
	} {
		// UI and protocol/harness runners are not task commands. Skip is
		// node-only, so explicitly cover their descendants as well.
		if path == runner || strings.HasPrefix(path, runner+" ") {
			return true
		}
	}
	return false
}

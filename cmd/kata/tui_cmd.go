package main

import (
	"context"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/tui"
)

// newTUICmd registers the TUI command. --all-projects is intentionally
// absent today: the daemon has no cross-project list endpoint
// (handlers_issues.go only registers the project-scoped route), so
// advertising the flag would land the user on a 404. The all-projects
// surface is gated end-to-end (this flag, the R toggle, and the boot-
// fallback path) until the daemon ships GET /issues for cross-project
// reads.
func newTUICmd() *cobra.Command {
	var uidFormat string
	var mouse bool
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "open the interactive issue browser",
		Long: `kata tui opens a Bubble Tea TUI scoped to the current project (per .kata.toml).
Press ? for help, q to quit.

Mouse support is opt-in. Set [tui] mouse = true in <KATA_HOME>/config.toml
or pass --mouse for one run. Hold Option (macOS) or Shift (Linux) for native
terminal text selection while mouse tracking is enabled.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if currentOutputMode() == outputAgent {
				return &cliError{Message: "kata tui does not support --agent; run without output formatting", Kind: kindUsage, ExitCode: ExitUsage}
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if !validTUIUIDFormat(uidFormat) {
				return &cliError{
					Message:  "uid format must be one of none, short, full",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			mouseEnabled, err := resolveTUIMouseOption(cmd, mouse)
			if err != nil {
				return err
			}
			return tui.Run(ctx, tui.Options{
				Stdout:           cmd.OutOrStdout(),
				Stderr:           cmd.ErrOrStderr(),
				DisplayUIDFormat: uidFormat,
				Mouse:            mouseEnabled,
			})
		},
	}
	cmd.Flags().StringVar(&uidFormat, "uid-format", "none", "show issue UIDs in detail (none|short|full)")
	cmd.Flags().BoolVar(&mouse, "mouse", false, "enable opt-in mouse support for this run (or set [tui] mouse = true in config.toml)")
	return cmd
}

func resolveTUIMouseOption(cmd *cobra.Command, flagValue bool) (bool, error) {
	cfg, err := config.ReadDaemonConfig()
	if err != nil {
		return false, err
	}
	if cmd.Flags().Changed("mouse") {
		return flagValue, nil
	}
	return cfg.TUI.Mouse, nil
}

func validTUIUIDFormat(v string) bool {
	switch v {
	case "none", "short", "full":
		return true
	default:
		return false
	}
}

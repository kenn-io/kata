package main

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/tui"
)

var runTUI = tui.Run

func newTUICmd() *cobra.Command {
	var uidFormat string
	var mouse bool
	cmd := &cobra.Command{
		Use:   "tui [issue-ref]",
		Short: "browse issues, jobs, runs and workflows",
		Long: `kata tui opens native Kata / Jobs / Runs / Workflows tabs for the selected
daemon and current project (per .kata.toml). Kata is the default issue browser.
Press 1–4 to select a tab; forms and prompts keep their numeric input.
Pass an optional issue ref to open that issue's detail view directly.

Jobs define a trigger and action. Workflows contain reusable command or prompt
steps with dependencies and retries. Runs show recorded status and evidence.
Use kata cron --help for daemon delivery and external adapter requirements.

Create a workflow with kata cron workflow create --file workflow.json --json.
Run kata cron workflow create --help for the complete JSON example and retry rules.
In Jobs, n creates a job and e edits one; in Workflows, e edits an existing
definition in $EDITOR. Use kata cron job create --help to reference a workflow.

Press ? for issue help on Kata; cron screens show their keys in the footer.
Press q to quit.

Mouse support is opt-in. Set [tui] mouse = true in <KATA_HOME>/config.toml
or pass --mouse for one run. Hold Option (macOS) or Shift (Linux) for native
terminal text selection while mouse tracking is enabled.

Set [tui] confirm_quit = false in <KATA_HOME>/config.toml to make q quit immediately.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			tuiConfig, err := resolveTUIConfig(cmd, mouse)
			if err != nil {
				return err
			}
			var initialIssueRef string
			if len(args) == 1 {
				initialIssueRef = args[0]
			}
			workspace := strings.TrimSpace(flags.Workspace)
			if workspace != "" {
				workspace, err = resolveStartPath(workspace)
				if err != nil {
					return err
				}
			}
			return runTUI(ctx, tui.Options{
				OpenURL:          platformOpenTarget,
				Stdout:           cmd.OutOrStdout(),
				Stderr:           cmd.ErrOrStderr(),
				DisplayUIDFormat: uidFormat,
				DaemonName:       flags.Daemon,
				Mouse:            tuiConfig.Mouse,
				SkipQuitConfirm:  tuiConfig.ConfirmQuit != nil && !*tuiConfig.ConfirmQuit,
				InitialIssueRef:  initialIssueRef,
				ProjectName:      strings.TrimSpace(flags.Project),
				Workspace:        workspace,
			})
		},
	}
	cmd.Flags().StringVar(&uidFormat, "uid-format", "none", "show issue UIDs in detail (none|short|full)")
	cmd.Flags().BoolVar(&mouse, "mouse", false, "enable opt-in mouse support for this run (or set [tui] mouse = true in config.toml)")
	return cmd
}

func resolveTUIConfig(cmd *cobra.Command, flagValue bool) (config.TUIConfig, error) {
	cfg, err := config.ReadTUIConfig()
	if err != nil {
		return config.TUIConfig{}, err
	}
	if cmd.Flags().Changed("mouse") {
		cfg.Mouse = flagValue
	}
	return cfg, nil
}

func validTUIUIDFormat(v string) bool {
	switch v {
	case "none", "short", "full":
		return true
	default:
		return false
	}
}

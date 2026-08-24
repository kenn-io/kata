package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newPurgeCmd returns the cobra.Command for `kata purge`.
//
// Spec §3.5 step 5: purge is irreversible and gated by --force plus an
// X-Kata-Confirm header whose value is the exact string "PURGE <ref>". The
// interactive friction is intentionally higher than delete — the prompt
// requires typing the full "PURGE <ref>" string, not just the ref.
func newPurgeCmd() *cobra.Command {
	var force bool
	var confirm string
	var reason string
	cmd := &cobra.Command{
		Use:   "purge <issue-ref>",
		Short: "irreversibly remove an issue + all its rows",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				return &cliError{
					Message: "purge requires --force; this is irreversible",
					Code:    "validation",
					Kind:    kindValidation, ExitCode: ExitValidation,
				}
			}
			ctx, baseURL, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			// Resolve to the qualified display form so X-Kata-Confirm
			// works whether the user passed a ULID or a short_id.
			issue, err = hydrateRefWithQualified(ctx, baseURL, pid, issue, true)
			if err != nil {
				return err
			}
			expected := fmt.Sprintf("PURGE %s", issue.QualifiedID)
			confirm, err = resolveConfirm(cmd, confirm, expected,
				fmt.Sprintf("Type %q to confirm: ", expected))
			if err != nil {
				return err
			}
			var extra map[string]any
			if reason != "" {
				extra = map[string]any{"reason": reason}
			}
			return runDestructive(cmd, baseURL, pid, issue.RefForAPI, issue.QualifiedID, "purge", confirm, extra)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "required to perform the purge")
	cmd.Flags().StringVar(&confirm, "confirm", "", `exact confirmation string ("PURGE <short_id>")`)
	cmd.Flags().StringVar(&reason, "reason", "", "free-text reason recorded in purge_log.reason")
	return cmd
}

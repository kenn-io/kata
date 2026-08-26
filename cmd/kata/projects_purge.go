package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

// projectsPurgeCmd permanently deletes an archived project and frees its name.
// Gated by --force plus an X-Kata-Confirm header whose value is the exact
// string "PURGE <project>", mirroring `kata purge` for issues.
func projectsPurgeCmd() *cobra.Command {
	var force bool
	var confirm string
	var reason string
	cmd := &cobra.Command{
		Use:   "purge <project>",
		Short: "permanently delete an archived project (irreversible; frees the name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				return &cliError{
					Message:  "purge requires --force; this is irreversible",
					Code:     "validation",
					Kind:     kindValidation,
					ExitCode: ExitValidation,
				}
			}
			ctx := cmd.Context()
			a, err := dialDaemon(ctx)
			if err != nil {
				return err
			}
			project, err := resolveProjectSelectorIncludingArchived(a, args[0])
			if err != nil {
				return err
			}
			expected := fmt.Sprintf("PURGE %s", project.Name)
			confirm, err = resolveConfirm(cmd, confirm, expected,
				fmt.Sprintf("Type %q to confirm: ", expected))
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			body := map[string]any{"actor": actor}
			if reason != "" {
				body["reason"] = reason
			}
			bs, err := a.doWithHeaders(http.MethodPost,
				fmt.Sprintf("/api/v1/projects/%d/actions/purge", project.ID),
				map[string]string{"X-Kata-Confirm": confirm}, body)
			if err != nil {
				return err
			}
			return printProjectPurge(cmd, project.Name, bs)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "required to perform the purge")
	cmd.Flags().StringVar(&confirm, "confirm", "", `exact confirmation string ("PURGE <project>")`)
	cmd.Flags().StringVar(&reason, "reason", "", "free-text reason recorded in the audit tombstone")
	return cmd
}

func printProjectPurge(cmd *cobra.Command, name string, bs []byte) error {
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		return writeAgentProjectAction(cmd.OutOrStdout(), "purge",
			agentRowField("project", name),
		)
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"%s purged (irreversible); name is now free\n", textsafe.Line(name))
	return err
}

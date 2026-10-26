package main

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"
)

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "show resolved actor, teammate, and sources",
		Long: `Print the client-resolved actor and source (--as, KATA_AUTHOR, USER,
or git). It does not query daemon authentication. For the effective actor
and current issue owner on an authenticated daemon, use kata status <ref>.

Teammate attribution: --teammate > KATA_TEAMMATE > harness inference.
An explicit empty teammate suppresses both defaults. Output includes teammate,
teammate_source (flag, env, inferred, ambiguous, disabled, or none), and harness.
Inference never changes inbox or reply addressing.`,
		Example: `  kata whoami --agent`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			actor, source := resolveActor(ctx, flags.As, nil)
			teammate, err := resolveTeammate(cmd)
			if err != nil {
				return err
			}
			mode := currentOutputMode()
			if mode == outputAgent {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK whoami actor=%s source=%s teammate=%s teammate_source=%s harness=%s\n",
					agentValue(actor), agentValue(source), agentValue(teammate.Handle), agentValue(teammate.Source), agentValue(teammate.Harness))
				return err
			}
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, map[string]string{
					"actor": actor, "source": source,
					"teammate": teammate.Handle, "teammate_source": teammate.Source, "harness": teammate.Harness,
				}); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "actor=%s source=%s teammate=%s teammate_source=%s harness=%s\n",
				actor, source, teammate.Handle, teammate.Source, teammate.Harness)
			return err
		},
	}
}

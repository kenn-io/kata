package main

import (
	"bytes"
	"fmt"

	"github.com/spf13/cobra"
)

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "show resolved actor and source",
		Long: `Print the client-resolved actor and source (--as, KATA_AUTHOR, USER,
or git). It does not query daemon authentication. For the effective actor
and current issue owner on an authenticated daemon, use kata status <ref>.`,
		Example: `  kata whoami --agent`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			actor, source := resolveActor(ctx, flags.As, nil)
			mode := currentOutputMode()
			if mode == outputAgent {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK whoami actor=%s source=%s\n",
					agentValue(actor), agentValue(source))
				return err
			}
			if mode == outputJSON {
				var buf bytes.Buffer
				if err := emitJSON(&buf, map[string]string{"actor": actor, "source": source}); err != nil {
					return err
				}
				_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "actor=%s source=%s\n", actor, source)
			return err
		},
	}
}

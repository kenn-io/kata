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
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor, source := resolveActor(flags.As, nil)
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

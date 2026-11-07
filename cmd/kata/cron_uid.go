package main

import (
	"github.com/spf13/cobra"
	kataclient "go.kenn.io/kata/pkg/client"
)

// newCronNewUIDCmd generates a request identity offline. It submits nothing;
// callers retain the UID and pass it to request commands, including on retry.
func newCronNewUIDCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "new-uid",
		Short:   "generate a fresh ULID for a cron request without contacting the daemon",
		Long:    "Generate a fresh uppercase ULID locally. Retain it and pass it as --request-uid, reusing it after a lost response. Generation submits no request.",
		Example: "  kata cron new-uid --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := kataclient.NewCronUID()
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), struct {
				UID string `json:"uid"`
			}{id})
		},
	}
}

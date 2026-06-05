package main

import (
	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/daemon"
)

func newOpenAPICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "openapi",
		Short: "print the daemon's OpenAPI 3.1 schema (YAML)",
		Long: `Print the daemon's OpenAPI 3.1 schema as YAML to stdout.

The schema is generated in-process from the daemon's route definitions; it does
not require a running daemon or a database. Redirect it to a file to generate a
typed client or feed other tooling, e.g.:

    kata openapi > openapi.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := daemon.OpenAPIYAML()
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc)
			return err
		},
	}
}

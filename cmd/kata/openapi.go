package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/daemon"
)

func newOpenAPICmd() *cobra.Command {
	var version string
	var format string
	cmd := &cobra.Command{
		Use:   "openapi",
		Short: "print the daemon's OpenAPI schema",
		Long: `Print the daemon's OpenAPI schema to stdout.

The schema is generated in-process from the daemon's route definitions; it does
not require a running daemon or a database. Redirect it to a file to generate a
typed client or feed other tooling, e.g.:

    kata openapi > openapi.yaml
    kata openapi --version 3.0 --format yaml > pkg/client/openapi.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := renderOpenAPI(version, format)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(doc)
			return err
		},
	}
	cmd.Flags().StringVar(&version, "version", "3.1", "OpenAPI version to print: 3.1 or 3.0")
	cmd.Flags().StringVar(&format, "format", "yaml", "OpenAPI format to print: yaml or json")
	return cmd
}

func renderOpenAPI(version, format string) ([]byte, error) {
	switch format {
	case "yaml", "yml":
		return daemon.OpenAPIYAMLVersion(version)
	case "json":
		return daemon.OpenAPIJSONVersion(version)
	default:
		return nil, fmt.Errorf("unsupported openapi format %q", format)
	}
}

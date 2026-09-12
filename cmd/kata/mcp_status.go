package main

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/mcpdiscovery"
)

func newMCPStatusCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "status",
		Short: "List running HTTP MCP listeners",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			home, err := config.KataHome()
			if err != nil {
				return err
			}
			directory := filepath.Join(home, "mcp")
			endpoints, err := mcpdiscovery.List(directory)
			if err != nil {
				return err
			}
			if currentOutputMode() == outputJSON {
				return json.MarshalWrite(command.OutOrStdout(), endpoints)
			}
			if len(endpoints) == 0 {
				_, err := fmt.Fprintln(command.OutOrStdout(), "No HTTP MCP listeners are running.")
				return err
			}
			for _, endpoint := range endpoints {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "MCP %s (pid %d)\n", endpoint.URL, endpoint.PID); err != nil {
					return err
				}
			}
			return nil
		},
	}
	return command
}

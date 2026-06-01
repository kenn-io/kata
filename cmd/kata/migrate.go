package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
)

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "migrate",
		Short:         "bring the kata database up to the current schema",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dbPath, err := config.KataDSN(ctx)
			if err != nil {
				return err
			}
			store, result, err := storeopen.Open(ctx, dbPath, db.ApplyMigrations())
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			out := cmd.OutOrStdout()
			for _, v := range result.Applied {
				if _, err := fmt.Fprintf(out, "applied schema_version %d\n", v); err != nil {
					return err
				}
			}
			if len(result.Applied) == 0 {
				_, err := fmt.Fprintf(out, "already current (schema_version %d)\n", result.To)
				return err
			}
			_, err = fmt.Fprintf(out, "migrated from %d to %d (%d versions applied)\n",
				result.From, result.To, len(result.Applied))
			return err
		},
	}
	return cmd
}

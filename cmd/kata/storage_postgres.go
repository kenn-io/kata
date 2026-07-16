package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
)

type postgresSchemaOutput struct {
	Action        string `json:"action"`
	Backend       string `json:"backend"`
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
}

func newStorageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "manage daemon storage",
	}
	cmd.AddCommand(newStoragePostgresCmd())
	return cmd
}

func newStoragePostgresCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "postgres",
		Short: "manage a Postgres schema",
	}
	cmd.AddCommand(newStoragePostgresMigrateCmd(), newStoragePostgresStatusCmd())
	return cmd
}

func newStoragePostgresMigrateCmd() *cobra.Command {
	var dsn, schema string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "install or advance the Postgres schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, pgConfig, err := resolvePostgresCommandConfig(cmd, dsn, schema)
			if err != nil {
				return err
			}
			pgConfig.SchemaMode = pgstore.SchemaModeBootstrap
			store, err := pgstore.OpenWithConfig(cmd.Context(), dsn, pgConfig)
			if err != nil {
				return err
			}
			version, versionErr := store.SchemaVersion(cmd.Context())
			closeErr := store.Close()
			if versionErr != nil {
				return versionErr
			}
			if closeErr != nil {
				return closeErr
			}
			return emitPostgresSchemaOutput(cmd, postgresSchemaOutput{
				Action: "migrate", Backend: "postgres", Schema: pgConfig.Schema,
				SchemaVersion: version, Status: "ready",
			})
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (default: resolved KATA_DSN/storage config)")
	cmd.Flags().StringVar(&schema, "schema", "", "Postgres schema (default: configured schema or kata)")
	return cmd
}

func newStoragePostgresStatusCmd() *cobra.Command {
	var dsn, schema string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "validate Postgres schema readiness without DDL",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, pgConfig, err := resolvePostgresCommandConfig(cmd, dsn, schema)
			if err != nil {
				return err
			}
			pgConfig.SchemaMode = pgstore.SchemaModeValidate
			store, err := pgstore.OpenWithConfig(cmd.Context(), dsn, pgConfig, db.ReadOnly())
			if err != nil {
				return err
			}
			version, versionErr := store.SchemaVersion(cmd.Context())
			closeErr := store.Close()
			if versionErr != nil {
				return versionErr
			}
			if closeErr != nil {
				return closeErr
			}
			return emitPostgresSchemaOutput(cmd, postgresSchemaOutput{
				Action: "status", Backend: "postgres", Schema: pgConfig.Schema,
				SchemaVersion: version, Status: "ready",
			})
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "Postgres DSN (default: resolved KATA_DSN/storage config)")
	cmd.Flags().StringVar(&schema, "schema", "", "Postgres schema (default: configured schema or kata)")
	return cmd
}

func resolvePostgresCommandConfig(cmd *cobra.Command, dsn, schema string) (string, pgstore.Config, error) {
	var err error
	if dsn = strings.TrimSpace(dsn); dsn == "" {
		dsn, err = config.KataDSN(cmd.Context())
		if err != nil {
			return "", pgstore.Config{}, err
		}
	}
	backend, err := storeopen.BackendForDSN(dsn)
	if err != nil {
		return "", pgstore.Config{}, err
	}
	if backend != storeopen.BackendPostgres {
		return "", pgstore.Config{}, &cliError{
			Message: "storage postgres requires a postgres DSN",
			Kind:    kindUsage, ExitCode: ExitUsage,
		}
	}
	storageConfig, err := config.KataPostgresStorageConfig(cmd.Context())
	if err != nil {
		return "", pgstore.Config{}, err
	}
	pgConfig := pgstore.ConfigFromValues(
		storageConfig.Schema, storageConfig.Mode, storageConfig.SchemaOwner,
		storageConfig.AllowInsecure,
	)
	if schema = strings.TrimSpace(schema); schema != "" {
		pgConfig.Schema = schema
	}
	if err := pgConfig.Validate(); err != nil {
		return "", pgstore.Config{}, err
	}
	return dsn, pgConfig, nil
}

func emitPostgresSchemaOutput(cmd *cobra.Command, out postgresSchemaOutput) error {
	switch currentOutputMode() {
	case outputJSON:
		return emitJSON(cmd.OutOrStdout(), out)
	case outputAgent:
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"OK postgres_schema action=%s backend=%s schema=%s schema_version=%d status=%s\n",
			agentValue(out.Action), agentValue(out.Backend), agentValue(out.Schema),
			out.SchemaVersion, agentValue(out.Status))
		return err
	default:
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Postgres schema %q is ready at version %d.\n",
			out.Schema, out.SchemaVersion)
		return err
	}
}

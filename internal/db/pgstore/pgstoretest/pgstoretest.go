// Package pgstoretest gives tests PostgreSQL databases that already hold the
// Kata schema. Installing the schema costs more than the rest of most small
// storage tests, so each database is a clone of a template that installs it
// once per server.
//
// Tests that exercise schema installation itself, need an empty database, or
// need optional extensions keep using testenv.NewPostgresContainer.
package pgstoretest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

var standard = NewTemplate(pgstore.DefaultSchema)

// NewDatabase returns a DSN for an isolated database holding the schema
// under pgstore.DefaultSchema, plus a cleanup function that drops it. The
// first pgstore.Open on the database seeds the same rows and IDs as opening
// an empty database, including its own instance identity.
//
//nolint:revive // t is the canonical first arg for testing helpers
func NewDatabase(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	return standard.NewDatabase(t, ctx)
}

// NewTemplate returns a template whose clones hold the schema under the
// named PostgreSQL schema. Callers should create one per schema name and
// reuse it, because each template is built once per server.
func NewTemplate(schema string) *testenv.PostgresTemplate {
	return testenv.NewPostgresTemplate(func(ctx context.Context, dsn string) error {
		config := pgstore.DefaultConfig()
		config.Schema = schema
		store, err := pgstore.OpenWithConfig(ctx, dsn, config)
		if err != nil {
			return fmt.Errorf("install schema %q: %w", schema, err)
		}
		if err := store.Close(); err != nil {
			return fmt.Errorf("close schema installer: %w", err)
		}
		return removeOpenSeeds(ctx, dsn, schema)
	})
}

// removeOpenSeeds deletes the rows Open seeds after installing the schema:
// the instance identity, which clones must not share, and the system
// project. Restarting the project IDs makes a clone's first Open assign the
// same IDs as a fresh install, because reseeding an existing system project
// still consumes an ID.
func removeOpenSeeds(ctx context.Context, dsn, schema string) error {
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open schema template: %w", err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.ExecContext(ctx, //nolint:gosec // G202: sanitized test schema identifier.
		`DELETE FROM `+pgx.Identifier{schema, "meta"}.Sanitize()+` WHERE key IN ('instance_uid', $1)`,
		db.MetaKeyInstanceCreatedAt); err != nil {
		return fmt.Errorf("clear template instance identity: %w", err)
	}
	if _, err := database.ExecContext(ctx,
		`TRUNCATE `+pgx.Identifier{schema, "projects"}.Sanitize()+` RESTART IDENTITY CASCADE`); err != nil {
		return fmt.Errorf("clear template system project: %w", err)
	}
	return nil
}

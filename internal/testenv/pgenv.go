package testenv

import (
	"context"
	"testing"

	"go.kenn.io/kata/internal/testpg"
)

// NewPostgresContainer returns an isolated database on a shared PostgreSQL
// server. Callers must register the cleanup with t.Cleanup. See testpg for
// explicit-server and automatic-container behavior.
//
//nolint:revive // t is the canonical first arg for testing helpers
func NewPostgresContainer(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	return testpg.NewPostgresContainer(t, ctx)
}

// NewPostgresWithPgvectorContainer returns an isolated database with pgvector.
//
//nolint:revive // t is the canonical first arg for testing helpers
func NewPostgresWithPgvectorContainer(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	return testpg.NewPostgresWithPgvectorContainer(t, ctx)
}

// NewPlainPostgresContainer returns an isolated database without extensions.
//
//nolint:revive // t is the canonical first arg for testing helpers
func NewPlainPostgresContainer(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	return testpg.NewPlainPostgresContainer(t, ctx)
}

// PostgresTemplate gives each test an isolated clone of a prepared database.
type PostgresTemplate = testpg.PostgresTemplate

// NewPostgresTemplate prepares a reusable template once per PostgreSQL server.
func NewPostgresTemplate(prepare func(ctx context.Context, dsn string) error) *PostgresTemplate {
	return testpg.NewPostgresTemplate(prepare)
}

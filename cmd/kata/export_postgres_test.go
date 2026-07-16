package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/testenv"
)

func TestExportReadsPostgresThroughConfiguredDSN(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_DSN", dsn)

	store, err := storeopen.Open(ctx, dsn)
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "postgres-export")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "exported from postgres",
		Author:    "fixture-author",
	})
	require.NoError(t, err)
	require.NoError(t, store.Close())

	output := filepath.Join(home, "postgres-export.jsonl")
	_, err = runCmdOutput(t, nil, "export", "--output", output)
	require.NoError(t, err)
	body, err := os.ReadFile(output) //nolint:gosec // test-owned output path
	require.NoError(t, err)
	assert.Contains(t, string(body), `"name":"postgres-export"`)
	assert.Contains(t, string(body), `"title":"exported from postgres"`)
}

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestExportWritesJSONLToOutput(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	p, err := d.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	_, _, err = d.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "exported issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	outPath := filepath.Join(home, "export.jsonl")
	out, err := runCmdOutput(t, nil, "export", "--output", outPath)
	require.NoError(t, err)

	bs, err := os.ReadFile(outPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(bs), `"kind":"meta"`)
	assert.Contains(t, string(bs), "exported issue")
	assert.Contains(t, out, outPath)
}

func TestExportAgentOutput(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	p, err := d.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	_, _, err = d.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "agent export",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	outPath := filepath.Join(home, "agent.jsonl")
	out, err := runCmdOutput(t, nil, "export", "--agent", "--output", outPath)
	require.NoError(t, err)

	assert.Equal(t, "OK export output="+outPath+"\n", out)
}

func TestExportScopesByProjectName(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	ctx := context.Background()
	d, err := db.Open(ctx, dbPath)
	require.NoError(t, err)
	alpha, err := d.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := d.CreateProject(ctx, "beta")
	require.NoError(t, err)
	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "alpha-only", Author: "tester"})
	require.NoError(t, err)
	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: beta.ID, Title: "beta-only", Author: "tester"})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	outPath := filepath.Join(home, "alpha.jsonl")
	_, err = runCmdOutput(t, nil, "--project", "alpha", "export", "--output", outPath)
	require.NoError(t, err)
	bs, err := os.ReadFile(outPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(bs), "alpha-only")
	assert.NotContains(t, string(bs), "beta-only")
}

func TestExportProjectNameNotFound(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	_, err = d.CreateProject(context.Background(), "alpha")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	_, err = runCmdOutput(t, nil, "--project", "nope", "export", "--output", filepath.Join(home, "x.jsonl"))
	ce := requireCLIError(t, err, ExitNotFound)
	assert.Contains(t, ce.Message, `project "nope" not found`)
}

func TestExportProjectFlagConflict(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	alpha, err := d.CreateProject(context.Background(), "alpha")
	require.NoError(t, err)
	beta, err := d.CreateProject(context.Background(), "beta")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	_, err = runCmdOutput(t, nil,
		"--project", "alpha", "export",
		"--project-id", fmt.Sprintf("%d", beta.ID),
		"--output", filepath.Join(home, "x.jsonl"))
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "conflicts with --project-id")
	_ = alpha
}

func TestExportRefusesRunningDaemonUnlessAllowed(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	require.NoError(t, writeRuntimeFor(home, addr))

	_, err = runCmdOutput(t, nil, "export", "--output", filepath.Join(home, "export.jsonl"))
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "daemon is running")
}

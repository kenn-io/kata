package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestExportWritesJSONLToOutput(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d := openKataTestDB(t, dbPath)
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

func TestExportReadsDatabaseWithoutWritePermission(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d := openKataTestDB(t, dbPath)
	p, err := d.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	_, _, err = d.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "read-only export",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	require.NoError(t, os.Chmod(dbPath, 0o400))
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o600) })

	outPath := filepath.Join(home, "readonly.jsonl")
	_, err = runCmdOutput(t, nil, "export", "--output", outPath)
	require.NoError(t, err)

	bs, err := os.ReadFile(outPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(bs), "read-only export")
}

func TestExportDoesNotReplaceExistingOutputOnFailure(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	ctx := context.Background()
	d := openKataTestDB(t, dbPath)
	p, err := d.CreateProject(ctx, "kata")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "invalid metadata",
		Author:    "tester",
	})
	require.NoError(t, err)
	conn, err := d.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE issues SET metadata = '{' WHERE id = ?`, issue.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = OFF`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	require.NoError(t, d.Close())

	outPath := filepath.Join(home, "export.jsonl")
	require.NoError(t, os.WriteFile(outPath, []byte("previous backup\n"), 0o600))
	_, err = runCmdOutput(t, nil, "export", "--output", outPath)
	require.Error(t, err)

	bs, readErr := os.ReadFile(outPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "previous backup\n", string(bs))
}

func TestExportReplaceOutputDoesNotDeleteExistingOutput(t *testing.T) {
	bs, err := os.ReadFile("export.go")
	require.NoError(t, err)

	re := regexp.MustCompile(`os\.Remove\(\s*output\s*\)`)
	assert.NotRegexp(t, re, string(bs),
		"export replacement must not delete the existing backup before replacement succeeds")
}

func TestExportReplaceOutputUsesWindowsReplacePrimitive(t *testing.T) {
	bs, err := os.ReadFile("export_replace_windows.go")
	require.NoError(t, err)

	assert.Contains(t, string(bs), "windows.MoveFileEx")
	assert.Contains(t, string(bs), "windows.MOVEFILE_REPLACE_EXISTING")
}

func TestReplaceExportOutputReplacesExistingOutput(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "export.jsonl")
	tmp := filepath.Join(dir, ".export.jsonl.tmp")
	require.NoError(t, os.WriteFile(output, []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(tmp, []byte("new\n"), 0o600))

	require.NoError(t, replaceExportOutput(tmp, output))

	bs, err := os.ReadFile(output) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(bs))
	_, err = os.Stat(tmp)
	assert.True(t, os.IsNotExist(err), "successful replacement must consume the temp file")
}

func TestExportAgentOutput(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d := openKataTestDB(t, dbPath)
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

	// agentValue is identity for separator-free Unix paths but quotes Windows
	// paths (backslashes), so assert against the formatted value, not raw.
	assert.Equal(t, "OK export output="+agentValue(outPath)+"\n", out)
}

func TestExportScopesByProjectName(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	ctx := context.Background()
	d := openKataTestDB(t, dbPath)
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
	d := openKataTestDB(t, dbPath)
	_, err := d.CreateProject(context.Background(), "alpha")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	_, err = runCmdOutput(t, nil, "--project", "nope", "export", "--output", filepath.Join(home, "x.jsonl"))
	ce := requireCLIError(t, err, ExitNotFound)
	assert.Contains(t, ce.Message, `project "nope" not found`)
}

func TestExportProjectFlagConflict(t *testing.T) {
	home := setupKataEnv(t)
	dbPath := filepath.Join(home, "kata.db")
	d := openKataTestDB(t, dbPath)
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
	d := openKataTestDB(t, dbPath)
	require.NoError(t, d.Close())
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	require.NoError(t, writeRuntimeFor(home, addr))

	_, err := runCmdOutput(t, nil, "export", "--output", filepath.Join(home, "export.jsonl"))
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "daemon is running")
}

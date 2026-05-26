package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func setupImportTest(t *testing.T) (home, input, target string) {
	t.Helper()
	home = setupKataEnv(t)
	input = writeExportFixture(t, home)
	target = filepath.Join(home, "target.db")
	return home, input, target
}

func TestImportCreatesTargetDB(t *testing.T) {
	_, input, target := setupImportTest(t)

	out, err := runCmdOutput(t, nil, "import", "--input", input, "--target", target)
	require.NoError(t, err)

	d, err := db.Open(context.Background(), target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	got, err := d.ProjectByName(context.Background(), "kata")
	require.NoError(t, err)
	assert.Equal(t, "kata", got.Name)
	assert.Contains(t, out, target)
}

func TestImportFormatAgentSelectsOutputMode(t *testing.T) {
	_, input, target := setupImportTest(t)

	out, err := runCmdOutput(t, nil, "import", "--format", "agent", "--source-format", "kata", "--input", input, "--target", target)
	require.NoError(t, err)

	d, err := db.Open(context.Background(), target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	got, err := d.ProjectByName(context.Background(), "kata")
	require.NoError(t, err)
	assert.Equal(t, "kata", got.Name)
	assert.Equal(t, "OK import source_format=kata target="+target+"\n", out)
}

func TestImportLegacyFormatConflictsWithSourceFormat(t *testing.T) {
	setupKataEnv(t)

	_, err := runCmdOutput(t, nil, "import", "--format", "beads", "--source-format", "kata")
	ce := requireCLIError(t, err, ExitUsage)
	assert.Contains(t, ce.Message, "--format beads cannot be combined with --source-format")
}

func TestImportLegacyFormatBeadsAllowsAgentOutputMode(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	setupKataEnv(t)

	_, stderr, err := executeRootCapture(t, context.Background(),
		"import", "--format", "beads", "--agent", "--input", "beads.jsonl")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "ERR import validation:"),
		"stderr should use agent mode for legacy beads import, got %q", stderr)
	assert.Contains(t, stderr, "--input is not supported")
}

func TestImportLegacyFormatBeadsParseErrorPreservesAgentMode(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	setupKataEnv(t)

	_, stderr, err := executeRootCapture(t, context.Background(),
		"import", "--format", "beads", "--agent", "--bogus")
	require.Error(t, err)
	assert.Truef(t, strings.HasPrefix(stderr, "ERR import usage:"),
		"stderr should use agent mode for legacy beads parse error, got %q", stderr)
}

func TestImportLegacyFormatBeadsParseErrorPreservesJSONMode(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	setupKataEnv(t)

	_, stderr, err := executeRootCapture(t, context.Background(),
		"import", "--format", "beads", "--json", "--bogus")
	require.Error(t, err)
	got := parseErrorEnvelope(t, []byte(stderr))
	assert.Equal(t, "usage", got.Error.Kind)
	assert.Contains(t, got.Error.Message, "unknown flag: --bogus")
}

func TestImportRejectsExistingTargetWithoutForce(t *testing.T) {
	_, input, target := setupImportTest(t)
	d, err := db.Open(context.Background(), target)
	require.NoError(t, err)
	_, err = d.CreateProject(context.Background(), "existing")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	_, err = runCmdOutput(t, nil, "import", "--input", input, "--target", target)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "target already exists")
}

func TestImportRefusesDaemon(t *testing.T) {
	home, input, target := setupImportTest(t)
	dbPath := filepath.Join(home, "kata.db")
	d, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, d.Close())
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	require.NoError(t, writeRuntimeFor(home, addr))

	_, err = runCmdOutput(t, nil, "import", "--input", input, "--target", target)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "daemon is running")
	assert.NotContains(t, ce.Message, "--allow-running-daemon")
}

func writeExportFixture(t *testing.T, home string) string {
	t.Helper()
	srcPath := filepath.Join(home, "source.db")
	src, err := db.Open(context.Background(), srcPath)
	require.NoError(t, err)
	p, err := src.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	_, _, err = src.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "imported issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, jsonl.Export(context.Background(), src, &out, jsonl.ExportOptions{IncludeDeleted: true}))
	require.NoError(t, src.Close())
	input := filepath.Join(home, "input.jsonl")
	require.NoError(t, os.WriteFile(input, out.Bytes(), 0o600))
	return input
}

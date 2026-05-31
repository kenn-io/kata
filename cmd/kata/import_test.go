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
	// agentValue quotes Windows paths (backslashes); assert the formatted value.
	assert.Equal(t, "OK import source_format=kata target="+agentValue(target)+"\n", out)
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

func TestImportRejectsExistingTargetSidecarWithoutForce(t *testing.T) {
	_, input, target := setupImportTest(t)
	require.NoError(t, os.WriteFile(target+"-wal", []byte("stale-wal"), 0o600))

	_, err := runCmdOutput(t, nil, "import", "--input", input, "--target", target)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "target already exists")

	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "non-force import must not install beside stale sidecars")
	gotWAL, readErr := os.ReadFile(target + "-wal") //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "stale-wal", string(gotWAL))
}

func TestInstallImportedTargetForceRemovesSidecarsWhenMainTargetIsMissing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	tmpTarget := filepath.Join(dir, "imported.db")
	require.NoError(t, os.WriteFile(tmpTarget, []byte("new-db"), 0o600))
	require.NoError(t, os.WriteFile(target+"-wal", []byte("stale-wal"), 0o600))
	require.NoError(t, os.WriteFile(target+"-shm", []byte("stale-shm"), 0o600))

	require.NoError(t, installImportedTarget(tmpTarget, target, true))

	gotTarget, readErr := os.ReadFile(target) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "new-db", string(gotTarget))
	_, statErr := os.Stat(target + "-wal")
	assert.True(t, os.IsNotExist(statErr), "force import must remove stale wal sidecar")
	_, statErr = os.Stat(target + "-shm")
	assert.True(t, os.IsNotExist(statErr), "force import must remove stale shm sidecar")
}

func TestInstallImportedTargetForcePreservesUserFileAtDeterministicBackupPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	tmpTarget := filepath.Join(dir, "imported.db")
	userFile := target + ".replace.tmp"
	require.NoError(t, os.WriteFile(target, []byte("old-db"), 0o600))
	require.NoError(t, os.WriteFile(tmpTarget, []byte("new-db"), 0o600))
	require.NoError(t, os.WriteFile(userFile, []byte("keep-me"), 0o600))

	require.NoError(t, installImportedTarget(tmpTarget, target, true))

	gotTarget, readErr := os.ReadFile(target) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "new-db", string(gotTarget))
	gotUserFile, readErr := os.ReadFile(userFile) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "keep-me", string(gotUserFile))
}

func TestInstallImportedTargetMovesTempSidecars(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	tmpTarget := filepath.Join(dir, "imported.db")
	require.NoError(t, os.WriteFile(tmpTarget, []byte("new-db"), 0o600))
	require.NoError(t, os.WriteFile(tmpTarget+"-wal", []byte("new-wal"), 0o600))
	require.NoError(t, os.WriteFile(tmpTarget+"-shm", []byte("new-shm"), 0o600))

	require.NoError(t, installImportedTarget(tmpTarget, target, false))

	gotTarget, readErr := os.ReadFile(target) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "new-db", string(gotTarget))
	gotWAL, readErr := os.ReadFile(target + "-wal") //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "new-wal", string(gotWAL))
	gotSHM, readErr := os.ReadFile(target + "-shm") //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "new-shm", string(gotSHM))
	_, statErr := os.Stat(tmpTarget + "-wal")
	assert.True(t, os.IsNotExist(statErr), "installed import must not leave wal sidecar at temp path")
	_, statErr = os.Stat(tmpTarget + "-shm")
	assert.True(t, os.IsNotExist(statErr), "installed import must not leave shm sidecar at temp path")
}

func TestImportForcePreservesExistingTargetOnFailure(t *testing.T) {
	home := setupKataEnv(t)
	input := filepath.Join(home, "bad.jsonl")
	require.NoError(t, os.WriteFile(input, []byte(`{"kind":"issue","data":{}}`+"\n"), 0o600))
	target := filepath.Join(home, "target.db")
	ctx := context.Background()
	d, err := db.Open(ctx, target)
	require.NoError(t, err)
	_, err = d.CreateProject(ctx, "existing")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	_, err = runCmdOutput(t, nil, "import", "--force", "--input", input, "--target", target)
	require.Error(t, err)

	d, err = db.Open(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.ProjectByName(ctx, "existing")
	require.NoError(t, err)
}

func TestImportFailureRemovesNewPartialTarget(t *testing.T) {
	home := setupKataEnv(t)
	input := filepath.Join(home, "bad.jsonl")
	require.NoError(t, os.WriteFile(input, []byte(`{"kind":"issue","data":{}}`+"\n"), 0o600))
	target := filepath.Join(home, "target.db")

	_, err := runCmdOutput(t, nil, "import", "--input", input, "--target", target)
	require.Error(t, err)

	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "failed import must not leave a partial target DB")
}

func TestInstallImportedTargetForcePreservesUserDirectoryAtDeterministicBackupSidecarPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	tmpTarget := filepath.Join(dir, "imported.db")
	backupWALDir := target + ".replace.tmp-wal"
	require.NoError(t, os.WriteFile(target, []byte("old-db"), 0o600))
	require.NoError(t, os.WriteFile(target+"-wal", []byte("old-wal"), 0o600))
	require.NoError(t, os.WriteFile(tmpTarget, []byte("new-db"), 0o600))
	require.NoError(t, os.Mkdir(backupWALDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(backupWALDir, "block"), []byte("x"), 0o600))

	require.NoError(t, installImportedTarget(tmpTarget, target, true))

	gotTarget, readErr := os.ReadFile(target) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "new-db", string(gotTarget))
	_, statErr := os.Stat(target + "-wal")
	assert.True(t, os.IsNotExist(statErr), "force import must remove the old target wal")
	gotBlock, readErr := os.ReadFile(filepath.Join(backupWALDir, "block")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "x", string(gotBlock))
}

func TestMoveSQLiteFileSetRollsBackAlreadyMovedSidecarOnError(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "from.db")
	to := filepath.Join(dir, "to.db")
	require.NoError(t, os.WriteFile(from+"-wal", []byte("wal"), 0o600))
	require.NoError(t, os.WriteFile(from+"-shm", []byte("shm"), 0o600))
	require.NoError(t, os.Mkdir(to+"-shm", 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(to+"-shm", "block"), []byte("x"), 0o600))

	moved, err := moveSQLiteFileSet(from, to)
	require.Error(t, err)
	assert.True(t, moved)

	gotWAL, readErr := os.ReadFile(from + "-wal") //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "wal", string(gotWAL))
	gotSHM, readErr := os.ReadFile(from + "-shm") //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, "shm", string(gotSHM))
	_, statErr := os.Stat(to + "-wal")
	assert.True(t, os.IsNotExist(statErr), "rolled back wal sidecar must not remain at destination")
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

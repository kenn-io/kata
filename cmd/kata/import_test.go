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

	d := openKataTestDB(t, target)
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

	d := openKataTestDB(t, target)
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
	d := openKataTestDB(t, target)
	_, err := d.CreateProject(context.Background(), "existing")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	_, err = runCmdOutput(t, nil, "import", "--input", input, "--target", target)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "target already exists")
}

func TestImportMergeRejectsReplacementFlags(t *testing.T) {
	setupKataEnv(t)
	for _, incompatible := range []string{"--force", "--new-instance"} {
		t.Run(incompatible, func(t *testing.T) {
			_, err := runCmdOutput(t, nil,
				"import", "--merge", incompatible, "--input", "snapshot.jsonl", "--target", "target.db")
			ce := requireCLIError(t, err, ExitValidation)
			assert.Contains(t, ce.Message, incompatible+" cannot be combined with --merge")
		})
	}
}

func TestImportMergeAfterPurgeAddsOneProjectWithoutChangingExistingProject(t *testing.T) {
	home := setupKataEnv(t)
	ctx := context.Background()

	sourcePath := filepath.Join(home, "source.db")
	source := openKataTestDB(t, sourcePath)
	importedProject, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	importedIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: importedProject.ID, Title: "imported issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	_, _, err = source.CreateComment(ctx, db.CreateCommentParams{
		IssueID: importedIssue.ID, Author: "fixture-author", Body: "imported comment",
	})
	require.NoError(t, err)
	_, err = source.AddLabel(ctx, importedIssue.ID, "portable", "fixture-author")
	require.NoError(t, err)
	purgedIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: importedProject.ID, Title: "purged issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	sourcePurge, err := source.PurgeIssue(ctx, purgedIssue.ID, "fixture-author", nil)
	require.NoError(t, err)
	require.NotNil(t, sourcePurge.PurgeResetAfterEventID)
	sourceEvents, err := source.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: importedProject.ID, Limit: 100,
	})
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &snapshot, jsonl.ExportOptions{
		ProjectID: importedProject.ID, IncludeDeleted: true,
	}))
	require.NoError(t, source.Close())
	input := filepath.Join(home, "spoke-project.jsonl")
	require.NoError(t, os.WriteFile(input, snapshot.Bytes(), 0o600))

	target := filepath.Join(home, "target.db")
	targetStore := openKataTestDB(t, target)
	existingProject, err := targetStore.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	existingIssue, _, err := targetStore.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: existingProject.ID, Title: "existing issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	targetEvents, err := targetStore.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: existingProject.ID, Limit: 100,
	})
	require.NoError(t, err)
	_, err = targetStore.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = 2000 WHERE name = 'issues'`)
	require.NoError(t, err)
	_, err = targetStore.ExecContext(ctx, `
		INSERT INTO project_purge_log(
			uid, origin_instance_uid, project_id, project_name,
			issue_count, event_count, alias_count, comment_count, link_count,
			label_count, claim_count, pending_claim_request_count,
			purge_reset_after_event_id, actor
		) VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, 0, 0, 0, 3000, ?)`,
		"01H00000000000000000000009", targetStore.InstanceUID(), existingProject.ID,
		existingProject.Name, "fixture-author")
	require.NoError(t, err)
	require.NoError(t, targetStore.Close())

	out, err := runCmdOutput(t, nil,
		"import", "--merge", "--input", input, "--target", target)
	require.NoError(t, err)
	assert.Contains(t, out, target)

	merged := openKataTestDB(t, target)
	t.Cleanup(func() { _ = merged.Close() })
	gotExisting, err := merged.ProjectByUID(ctx, existingProject.UID)
	require.NoError(t, err)
	assert.Equal(t, existingProject.Name, gotExisting.Name)
	gotExistingIssue, err := merged.IssueByUID(ctx, existingIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "existing issue", gotExistingIssue.Title)
	gotImported, err := merged.ProjectByUID(ctx, importedProject.UID)
	require.NoError(t, err)
	assert.NotEqual(t, importedProject.ID, gotImported.ID, "colliding numeric project ID must be remapped")
	assert.Equal(t, "spoke-project-2", gotImported.Name)
	gotImportedIssue, err := merged.IssueByUID(ctx, importedIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Greater(t, gotImportedIssue.ID, int64(2000), "merge must allocate above the target sequence high-water mark")
	assert.Equal(t, gotImported.ID, gotImportedIssue.ProjectID)
	assert.Equal(t, importedIssue.ShortID, gotImportedIssue.ShortID)
	comments, err := merged.CommentsByIssue(ctx, gotImportedIssue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "imported comment", comments[0].Body)
	labels, err := merged.LabelsByIssue(ctx, gotImportedIssue.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	assert.Equal(t, "portable", labels[0].Label)
	mergedEvents, err := merged.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: gotImported.ID, Limit: 100,
	})
	require.NoError(t, err)
	require.Len(t, mergedEvents, len(sourceEvents))
	for i := range sourceEvents {
		assert.Equal(t, sourceEvents[i].UID, mergedEvents[i].UID)
		if i > 0 {
			assert.Greater(t, mergedEvents[i].ID, mergedEvents[i-1].ID)
		}
	}
	require.NotEmpty(t, targetEvents)
	assert.Greater(t, mergedEvents[0].ID, targetEvents[len(targetEvents)-1].ID)
	assert.Greater(t, mergedEvents[0].ID, int64(3000), "merge events must remain visible beyond the purge reset cursor")
	var mergedSourceReset int64
	require.NoError(t, merged.QueryRowContext(ctx,
		`SELECT purge_reset_after_event_id FROM purge_log WHERE issue_uid = ?`, purgedIssue.UID).
		Scan(&mergedSourceReset))
	_, postMergeEvent, err := merged.CreateComment(ctx, db.CreateCommentParams{
		IssueID: gotImportedIssue.ID, Author: "fixture-author", Body: "after merge",
	})
	require.NoError(t, err)
	assert.Greater(t, postMergeEvent.ID, mergedSourceReset,
		"a post-merge event must remain visible beyond the imported purge reset cursor")
	require.NoError(t, merged.Close())

	_, err = runCmdOutput(t, nil,
		"import", "--merge", "--input", input, "--target", target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project UID")
	assert.Contains(t, err.Error(), "already exists")

	unchanged := openKataTestDB(t, target)
	t.Cleanup(func() { _ = unchanged.Close() })
	_, err = unchanged.IssueByUID(ctx, existingIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

func TestImportMergeRefusesMultiProjectSnapshotWithoutMutation(t *testing.T) {
	_, input, target := setupImportTest(t)
	ctx := context.Background()
	targetStore := openKataTestDB(t, target)
	existing, err := targetStore.CreateProject(ctx, "existing-project")
	require.NoError(t, err)
	require.NoError(t, targetStore.Close())

	_, err = runCmdOutput(t, nil,
		"import", "--merge", "--input", input, "--target", target)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project merge requires one non-system project")

	unchanged := openKataTestDB(t, target)
	t.Cleanup(func() { _ = unchanged.Close() })
	got, err := unchanged.ProjectByUID(ctx, existing.UID)
	require.NoError(t, err)
	assert.Equal(t, existing.Name, got.Name)
}

func TestImportMergeSkipsCrossProjectLink(t *testing.T) {
	home := setupKataEnv(t)
	ctx := context.Background()
	source := openKataTestDB(t, filepath.Join(home, "source-links.db"))
	importedProject, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	peerProject, err := source.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	importedIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: importedProject.ID, Title: "imported issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	peerIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: peerProject.ID, Title: "peer issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	_, err = source.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: importedIssue.ID, ToIssueID: peerIssue.ID,
		Type: "blocks", Author: "fixture-author",
	})
	require.NoError(t, err)
	var importedSnapshot, peerSnapshot bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &importedSnapshot, jsonl.ExportOptions{
		ProjectID: importedProject.ID, IncludeDeleted: true,
	}))
	require.NoError(t, jsonl.Export(ctx, source, &peerSnapshot, jsonl.ExportOptions{
		ProjectID: peerProject.ID, IncludeDeleted: true,
	}))
	require.NoError(t, source.Close())

	target := filepath.Join(home, "target-links.db")
	targetStore := openKataTestDB(t, target)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(peerSnapshot.Bytes()), targetStore,
		jsonl.ImportOptions{RequireFreshTarget: true}))
	require.NoError(t, targetStore.Close())
	input := filepath.Join(home, "spoke-links.jsonl")
	require.NoError(t, os.WriteFile(input, importedSnapshot.Bytes(), 0o600))

	_, err = runCmdOutput(t, nil,
		"import", "--merge", "--input", input, "--target", target)
	require.NoError(t, err)

	merged := openKataTestDB(t, target)
	t.Cleanup(func() { _ = merged.Close() })
	gotImported, err := merged.IssueByUID(ctx, importedIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	gotPeer, err := merged.IssueByUID(ctx, peerIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = merged.LinkByEndpoints(ctx, gotImported.ID, gotPeer.ID, "blocks")
	require.ErrorIs(t, err, db.ErrNotFound)
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
	d := openKataTestDB(t, target)
	_, err := d.CreateProject(ctx, "existing")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	_, err = runCmdOutput(t, nil, "import", "--force", "--input", input, "--target", target)
	require.Error(t, err)

	d = openKataTestDB(t, target)
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
	d := openKataTestDB(t, dbPath)
	require.NoError(t, d.Close())
	addr, cleanup := pipeServer(t)
	t.Cleanup(cleanup)
	require.NoError(t, writeRuntimeFor(home, addr))

	_, err := runCmdOutput(t, nil, "import", "--input", input, "--target", target)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "daemon is running")
	assert.NotContains(t, ce.Message, "--allow-running-daemon")
}

func writeExportFixture(t *testing.T, home string) string {
	t.Helper()
	srcPath := filepath.Join(home, "source.db")
	src := openKataTestDB(t, srcPath)
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

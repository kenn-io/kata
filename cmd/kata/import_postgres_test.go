package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestImportPostgresMergeAfterPurgeAddsOneProjectWithoutChangingExistingProject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	ctx := context.Background()
	source := openKataTestDB(t, filepath.Join(home, "source-merge.db"))
	importedProject, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	importedIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: importedProject.ID, Title: "imported issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	purgedIssue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: importedProject.ID, Title: "purged issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	sourcePurge, err := source.PurgeIssue(ctx, purgedIssue.ID, "fixture-author", nil)
	require.NoError(t, err)
	require.NotNil(t, sourcePurge.PurgeResetAfterEventID)
	var snapshot bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &snapshot, jsonl.ExportOptions{
		ProjectID: importedProject.ID, IncludeDeleted: true,
	}))
	require.NoError(t, source.Close())
	input := filepath.Join(home, "spoke-project.jsonl")
	require.NoError(t, os.WriteFile(input, snapshot.Bytes(), 0o600))

	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	target, err := storeopen.Open(ctx, dsn)
	require.NoError(t, err)
	existingProject, err := target.CreateProject(ctx, "existing-project")
	require.NoError(t, err)
	existingIssue, _, err := target.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: existingProject.ID, Title: "existing issue", Author: "fixture-author",
	})
	require.NoError(t, err)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('kata.issues','id'), 2000, true)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `
		INSERT INTO kata.project_purge_log(
			uid, origin_instance_uid, project_id, project_name,
			issue_count, event_count, alias_count, comment_count, link_count,
			label_count, claim_count, pending_claim_request_count,
			purge_reset_after_event_id, actor
		) VALUES ($1, $2, $3, $4, 0, 0, 0, 0, 0, 0, 0, 0, 3000, $5)`,
		"01H00000000000000000000009", target.InstanceUID(), existingProject.ID,
		existingProject.Name, "fixture-author")
	require.NoError(t, err)
	require.NoError(t, target.Close())

	_, err = runCmdOutput(t, nil,
		"import", "--merge", "--input", input, "--target", dsn)
	require.NoError(t, err)

	merged, err := storeopen.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = merged.Close() })
	gotExisting, err := merged.ProjectByUID(ctx, existingProject.UID)
	require.NoError(t, err)
	assert.Equal(t, existingProject.Name, gotExisting.Name)
	_, err = merged.IssueByUID(ctx, existingIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	gotImported, err := merged.ProjectByUID(ctx, importedProject.UID)
	require.NoError(t, err)
	gotImportedIssue, err := merged.IssueByUID(ctx, importedIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, gotImported.ID, gotImportedIssue.ProjectID)
	assert.Equal(t, importedIssue.ShortID, gotImportedIssue.ShortID)
	assert.Greater(t, gotImportedIssue.ID, int64(2000), "merge must allocate above the target sequence high-water mark")
	mergedEvents, err := merged.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: gotImported.ID, Limit: 100,
	})
	require.NoError(t, err)
	require.NotEmpty(t, mergedEvents)
	assert.Greater(t, mergedEvents[0].ID, int64(3000), "merge events must remain visible beyond the purge reset cursor")
	var mergedPurgedIssueID, mergedSourceReset int64
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT purged_issue_id, purge_reset_after_event_id FROM kata.purge_log WHERE issue_uid = $1`, purgedIssue.UID).
		Scan(&mergedPurgedIssueID, &mergedSourceReset))
	_, postMergeEvent, err := merged.CreateComment(ctx, db.CreateCommentParams{
		IssueID: gotImportedIssue.ID, Author: "fixture-author", Body: "after merge",
	})
	require.NoError(t, err)
	assert.Greater(t, postMergeEvent.ID, mergedSourceReset,
		"a post-merge event must remain visible beyond the imported purge reset cursor")
	postMergeIssue, _, err := merged.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: gotImported.ID, Title: "after merge", Author: "fixture-author",
	})
	require.NoError(t, err)
	assert.Greater(t, postMergeIssue.ID, mergedPurgedIssueID,
		"a post-merge issue must not reuse the imported tombstone's numeric ID")
}

func TestImportPostgresTargetCreatesThenAtomicallyReplacesSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	input := writeExportFixture(t, home)
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var schemaOwner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))
	runtimeRole := fmt.Sprintf("import_runtime_%d", os.Getpid())
	_, err = admin.ExecContext(ctx, fmt.Sprintf(
		`CREATE ROLE %s LOGIN PASSWORD 'runtime-password'`, runtimeRole,
	)) // #nosec G201 -- role is a fixed prefix plus process ID.
	require.NoError(t, err)
	runtimeDSN := postgresDSNForCLIUser(t, dsn, runtimeRole, "runtime-password")
	t.Setenv("KATA_DSN", runtimeDSN)
	// Production daemons use validation-only startup. Import receives the
	// schema-owner DSN explicitly and must bootstrap/replace independently of
	// that ambient runtime policy.
	t.Setenv("KATA_POSTGRES_SCHEMA_MODE", "validate")
	t.Setenv("KATA_POSTGRES_SCHEMA_OWNER", schemaOwner)
	wantTarget, err := config.CanonicalDSNIdentity(dsn)
	require.NoError(t, err)

	out, err := runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	require.NoError(t, err)
	assert.Contains(t, out, wantTarget)
	assert.NotContains(t, out, "kata:kata@",
		"success output must not expose Postgres userinfo")
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		REVOKE CREATE ON SCHEMA kata FROM PUBLIC;
		GRANT USAGE ON SCHEMA kata TO %s;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA kata TO %s;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA kata TO %s;
		GRANT EXECUTE ON FUNCTION kata.rewrite_project_uid_for_adoption(BIGINT, TEXT) TO %s;
	`, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID.
	require.NoError(t, err)

	store, err := storeopen.Open(ctx, runtimeDSN)
	require.NoError(t, err)
	project, err := store.ProjectByName(ctx, "kata")
	require.NoError(t, err)
	issues, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "imported issue", issues[0].Title)
	extra, err := store.CreateProject(ctx, "must-be-replaced")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	_, err = runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "target already exists")

	_, err = runCmdOutput(t, nil, "import", "--force", "--input", input, "--target", dsn)
	require.NoError(t, err)
	restored, err := storeopen.OpenReadOnly(ctx, runtimeDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restored.Close() })
	_, err = restored.ProjectByUID(ctx, extra.UID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestImportPostgresFailureRemovesFreshTargetSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	input := filepath.Join(home, "invalid.jsonl")
	require.NoError(t, os.WriteFile(input, []byte(`{"kind":"issue","data":{}}`+"\n"), 0o600))
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_POSTGRES_SCHEMA", "restore_store")

	_, err := runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	require.Error(t, err)
	version, peekErr := storeopen.PeekSchemaVersion(ctx, dsn)
	require.NoError(t, peekErr)
	assert.Zero(t, version, "failed fresh import must remove the target schema")
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'restore_store')`).Scan(&exists))
	assert.False(t, exists, "cleanup must remove the configured target schema")
}

func TestImportPostgresFailurePreservesPreexistingEmptySchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	home := setupKataEnv(t)
	input := filepath.Join(home, "invalid.jsonl")
	require.NoError(t, os.WriteFile(input, []byte(`{"kind":"issue","data":{}}`+"\n"), 0o600))
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_POSTGRES_SCHEMA", "preexisting_store")
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `CREATE SCHEMA preexisting_store`)
	require.NoError(t, err)

	_, err = runCmdOutput(t, nil, "import", "--input", input, "--target", dsn)
	require.Error(t, err)

	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'preexisting_store')`).Scan(&exists))
	assert.True(t, exists, "failed import must not drop a schema it did not create")
}

func TestFreshPostgresCleanupSurvivesCanceledImportContext(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	setupKataEnv(t)
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_POSTGRES_SCHEMA", "canceled_restore")
	store, err := storeopen.Open(ctx, dsn)
	require.NoError(t, err)
	uid := store.InstanceUID()
	require.NoError(t, store.Close())

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	pgConfig, err := postgresRestoreConfig(ctx)
	require.NoError(t, err)
	require.NoError(t, removeFreshPostgresTargetAfterFailure(canceled, dsn, uid, pgConfig))

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'canceled_restore')`).Scan(&exists))
	assert.False(t, exists)
}

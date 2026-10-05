package jsonl_test

import (
	"bytes"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"go.kenn.io/kata/internal/db/sqlitestore"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestProjectAccessBackupRestore(t *testing.T) {
	source := openExportTestDB(t)
	project, err := source.CreateProject(t.Context(), "restricted-project")
	require.NoError(t, err)
	access := db.ProjectAccessStorage(source)
	team, _, err := access.CreateTeam(t.Context(), "engineering", "admin")
	require.NoError(t, err)
	_, err = access.SetTeamMembership(t.Context(), team.UID, "member", true, "admin")
	require.NoError(t, err)
	policy, _, err := access.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{
		ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID},
	}, "admin")
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(backup.Bytes()), restored))
	target := db.ProjectAccessStorage(restored)
	got, err := target.ProjectAccessPolicy(t.Context(), project.UID)
	require.NoError(t, err)
	require.Equal(t, policy, got, "backup must not widen project visibility")
	members, err := target.TeamMembers(t.Context(), team.UID)
	require.NoError(t, err)
	require.Equal(t, []string{"member"}, members)
	visible, err := target.AccessibleProjectUIDs(t.Context(), "member")
	require.NoError(t, err)
	require.Equal(t, []string{project.UID}, visible)
	visible, err = target.AccessibleProjectUIDs(t.Context(), "outsider")
	require.NoError(t, err)
	require.Empty(t, visible)

	// Project transfer excludes owning-hub policy and membership objects.
	var scoped bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source, &scoped, jsonl.ExportOptions{ProjectID: project.ID}))
	require.NotContains(t, scoped.String(), `"project_access_revision"`, "owning-hub policy epoch must not enter a project transfer")
	for _, kind := range []string{`"kind":"team"`, `"kind":"team_membership"`, `"kind":"project_access_policy"`, `"kind":"project_access_team"`} {
		require.NotContains(t, scoped.String(), kind)
	}
}

func TestProjectAccessBackupAcrossPostgres(t *testing.T) {
	source := openExportTestDB(t)
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "restricted-project")
	require.NoError(t, err)
	team, _, err := source.CreateTeam(ctx, "engineering", "admin")
	require.NoError(t, err)
	_, err = source.SetTeamMembership(ctx, team.UID, "member", true, "admin")
	require.NoError(t, err)
	policy, _, err := source.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
	require.NoError(t, err)
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	postgres, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = postgres.Close() })
	// Names on independent hubs convey no authority or portable identity.
	other, _, err := postgres.CreateTeam(ctx, "engineering", "admin")
	require.NoError(t, err)
	require.NotEqual(t, team.UID, other.UID)
	var backup bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup.Bytes()), postgres))
	backup.Reset()
	require.NoError(t, jsonl.Export(ctx, postgres, &backup, jsonl.ExportOptions{IncludeDeleted: true}))
	target := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup.Bytes()), target))
	for _, store := range []db.Storage{postgres, target} {
		got, err := store.ProjectAccessPolicy(ctx, project.UID)
		require.NoError(t, err)
		require.Equal(t, policy, got)
		members, err := store.TeamMembers(ctx, team.UID)
		require.NoError(t, err)
		require.Equal(t, []string{"member"}, members)
		visible, err := store.AccessibleProjectUIDs(ctx, "outsider")
		require.NoError(t, err)
		require.Empty(t, visible)
		visible, err = store.AccessibleProjectUIDs(ctx, "member")
		require.NoError(t, err)
		require.Equal(t, []string{project.UID}, visible)
	}
}

// Released schema at e9f580c93fc167daeb8b60d11419cabae39b8c5f.
//
//go:embed testdata/schema_v30.sql
var projectAccessSQLiteSchema30 string

func TestProjectAccessCutoverUpgradesVersion30(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, projectAccessSQLiteSchema30+`
 INSERT INTO meta(key,value) VALUES('schema_version','30'),('instance_uid','00000000000000000000000001');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000002','existing-project');
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000003',id,'0003','Existing task','member' FROM projects WHERE name='existing-project';`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	visible, err := upgraded.AccessibleProjectUIDs(ctx, "member")
	require.NoError(t, err)
	require.Equal(t, []string{"00000000000000000000000002"}, visible)
	policy, err := upgraded.ProjectAccessPolicy(ctx, visible[0])
	require.NoError(t, err)
	require.Equal(t, "all", policy.Visibility)
	issue, err := upgraded.IssueByUID(ctx, "00000000000000000000000003", db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Existing task", issue.Title)
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
}

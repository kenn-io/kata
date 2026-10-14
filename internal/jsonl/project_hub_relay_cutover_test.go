package jsonl_test

import (
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	_ "modernc.org/sqlite"
)

// Released SQLite schema 32: main with comment replies, before project hub relay.
//
//go:embed testdata/schema_v32.sql
var projectHubRelaySQLiteSchema32 string

func TestProjectHubRelayCutoverUpgradesVersion32(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, projectHubRelaySQLiteSchema32+`
 INSERT INTO meta(key,value) VALUES('schema_version','32'),('instance_uid','00000000000000000000000009');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','existing-project');
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000002',id,'0002','Existing task','member' FROM projects WHERE name='existing-project';
 INSERT INTO federation_enrollments(token_hash,spoke_instance_uid,project_id,capabilities,bound_actor) SELECT replace(hex(zeroblob(32)),'00','bb'),'00000000000000000000000003',id,'pull,push','member' FROM projects WHERE name='existing-project';`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })

	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	visible, err := upgraded.AccessibleProjectUIDs(ctx, "member")
	require.NoError(t, err)
	require.Equal(t, []string{"00000000000000000000000001"}, visible)
	policy, err := upgraded.ProjectAccessPolicy(ctx, visible[0])
	require.NoError(t, err)
	require.Equal(t, "all", policy.Visibility)
	issue, err := upgraded.IssueByUID(ctx, "00000000000000000000000002", db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Existing task", issue.Title)
	enrollments, err := upgraded.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	require.Equal(t, 0, enrollments[0].RelayProtocolVersion, "an enrollment made before relay stays a legacy grant")
	require.Nil(t, enrollments[0].ParentTokenID)
}

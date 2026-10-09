package pgstore_test

import (
	"database/sql"
	_ "embed"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

//go:embed testdata/schema_v31.sql
var provenanceSchema31 string

func TestProvenanceMigrationUpgradesFrozenVersion31(t *testing.T) {
	require.Greater(t, db.CurrentSchemaVersion(), 31, "real next schema must exercise native migration")
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA provenance_upgrade;SET LOCAL search_path TO provenance_upgrade,pg_catalog,pg_temp;"+provenanceSchema31+"\nINSERT INTO meta(key,value) VALUES('schema_version','31'),('instance_uid','00000000000000000000000009');\nUPDATE meta SET value='47' WHERE key='project_access_revision';\nINSERT INTO projects(uid,name) VALUES('00000000000000000000000001','shared-project'),('00000000000000000000000002','empty-teams-project');\nINSERT INTO teams(uid,name,revision) VALUES('00000000000000000000000003','members',3);\nINSERT INTO team_memberships(team_uid,actor) VALUES('00000000000000000000000003','member');\nINSERT INTO project_access_policies(project_uid,visibility,revision) VALUES('00000000000000000000000001','teams',7),('00000000000000000000000002','teams',9);\nINSERT INTO project_access_teams(project_uid,team_uid) VALUES('00000000000000000000000001','00000000000000000000000003');\nINSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000004',id,'0004','Existing source task','assistant' FROM projects WHERE uid='00000000000000000000000001';\n")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "provenance_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })

	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	teams, err := upgraded.ListTeams(ctx)
	require.NoError(t, err)
	require.Equal(t, []db.Team{{UID: "00000000000000000000000003", Name: "members", Revision: 3}}, teams)
	members, err := upgraded.TeamMembers(ctx, teams[0].UID)
	require.NoError(t, err)
	require.Equal(t, []string{"member"}, members)
	policy, err := upgraded.ProjectAccessPolicy(ctx, "00000000000000000000000001")
	require.NoError(t, err)
	require.Equal(t, "teams", policy.Visibility)
	require.Equal(t, int64(7), policy.Revision)
	require.Equal(t, []string{teams[0].UID}, policy.TeamUIDs)
	empty, err := upgraded.ProjectAccessPolicy(ctx, "00000000000000000000000002")
	require.NoError(t, err)
	require.Equal(t, "teams", empty.Visibility)
	require.Empty(t, empty.TeamUIDs)
	require.Equal(t, int64(9), empty.Revision)
	visible, err := upgraded.AccessibleProjectUIDs(ctx, "outsider")
	require.NoError(t, err)
	require.Empty(t, visible)
	visible, err = upgraded.AccessibleProjectUIDs(ctx, "member")
	require.NoError(t, err)
	require.Equal(t, []string{"00000000000000000000000001"}, visible)
	epoch, err := upgraded.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(47), epoch)
	issue, err := upgraded.IssueByUID(ctx, "00000000000000000000000004", db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "Existing source task", issue.Title)
	require.Equal(t, "assistant", issue.Author)

	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "provenance_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	require.Equal(t, projectAccessStructure(t, fresh, "provenance_fresh"), projectAccessStructure(t, upgraded, "provenance_upgrade"))
}

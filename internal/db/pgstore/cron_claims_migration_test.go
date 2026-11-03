package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// Released 39 installs from the retained released schema plus immutable migrations.
func TestCronClaimsMigrationUpgrades39(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA claims_upgrade; SET LOCAL search_path TO claims_upgrade,pg_catalog,pg_temp;`+projectHubRelaySchema32)
	require.NoError(t, err)
	for _, m := range pgstore.Migrations() {
		if m.FromVersion >= 32 && m.ToVersion <= 39 {
			_, err = tx.ExecContext(ctx, m.SQL)
			require.NoError(t, err)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version','39'),('instance_uid','00000000000000000000000009'); INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','example-project');`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "claims_upgrade", SchemaMode: pgstore.SchemaModeValidate})
	require.Error(t, err, "runtime credentials must not migrate")
	s, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "claims_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	version, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	p, err := s.ProjectByUID(ctx, "00000000000000000000000001")
	require.NoError(t, err)
	require.Equal(t, "example-project", p.Name)
	def, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	j, _, err := s.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "worker", Definition: def})
	require.NoError(t, err)
	in := db.CronOccurrenceClaim{ProjectID: p.ID, JobUID: j.UID, DefinitionEventUID: j.DefinitionEventUID, RunUID: "00000000000000000000000003", HolderInstanceUID: s.InstanceUID(), Actor: "worker", OccurrenceKey: "manual:00000000000000000000000003"}
	out, err := s.ClaimCronOccurrence(ctx, in)
	require.NoError(t, err)
	require.False(t, out.Replayed)
	in.RunUID = "00000000000000000000000004"
	_, err = s.ClaimCronOccurrence(ctx, in)
	require.ErrorIs(t, err, db.ErrCronOccurrenceClaimed)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "claims_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	require.Equal(t, projectAccessStructure(t, fresh, "claims_fresh"), projectAccessStructure(t, s, "claims_upgrade"), "upgraded and fresh physical schemas agree")
}

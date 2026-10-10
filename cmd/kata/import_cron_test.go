package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

func TestInstallPreparedSQLiteImportKeepsOpenWriterAndExactContent(t *testing.T) {
	home := setupKataEnv(t)
	ctx := t.Context()
	path := filepath.Join(home, "target.db")
	target := openKataTestDB(t, path)
	_, err := target.CreateProject(ctx, "old-project")
	require.NoError(t, err)
	_, err = target.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('example-obsolete','old')`)
	require.NoError(t, err)
	require.NoError(t, target.Close())
	observer := openKataTestDB(t, path)
	defer func() { _ = observer.Close() }()
	beforeFile, err := os.Stat(path)
	require.NoError(t, err)
	preparedPath := filepath.Join(home, "prepared.db")
	prepared := openKataTestDB(t, preparedPath)
	project, err := prepared.CreateProject(ctx, "restored-project")
	require.NoError(t, err)
	definition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	_, _, err = prepared.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: definition, Actor: "worker"})
	require.NoError(t, err)
	expected, err := dbtest.CollectImportRecords(ctx, prepared, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	require.NoError(t, installPreparedSQLiteImport(ctx, preparedPath, path, true))
	afterFile, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(beforeFile, afterFile), "prepared installation must keep existing writers on the restored database")
	actual, err := dbtest.CollectImportRecords(ctx, observer, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	require.Equal(t, expected, actual, "prepared identity and content must not be transformed twice")
	_, err = observer.ExecContext(ctx, `UPDATE projects SET name='writer-after-restore' WHERE uid=$1`, project.UID)
	require.NoError(t, err)
	reopened := openKataTestDB(t, path)
	defer func() { _ = reopened.Close() }()
	got, err := reopened.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, "writer-after-restore", got.Name)
}

func TestImportForceSafeInstanceIdentity(t *testing.T) {
	for _, newInstance := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "new instance"}[newInstance], func(t *testing.T) {
			home, input, path := setupImportTest(t)
			source := openKataTestDB(t, filepath.Join(home, "source.db"))
			sourceUID := source.InstanceUID()
			require.NoError(t, source.Close())
			target := openKataTestDB(t, path)
			oldUID := target.InstanceUID()
			require.NoError(t, target.Close())
			args := []string{"import", "--force", "--input", input, "--target", path}
			if newInstance {
				args = append(args, "--new-instance")
			}
			_, err := runCmdOutput(t, nil, args...)
			require.NoError(t, err)
			restored := openKataTestDB(t, path)
			defer func() { _ = restored.Close() }()
			if newInstance {
				require.NotEqual(t, sourceUID, restored.InstanceUID())
				require.NotEqual(t, oldUID, restored.InstanceUID())
			} else {
				require.Equal(t, sourceUID, restored.InstanceUID())
			}
		})
	}
}

func TestInstallPreparedSQLiteImportInspectsTargetWithoutUpgrading(t *testing.T) {
	for _, version := range []string{"30", "32"} {
		t.Run(version, func(t *testing.T) {
			home := setupKataEnv(t)
			targetPath := filepath.Join(home, "target.db")
			target := openKataTestDB(t, targetPath)
			_, err := target.ExecContext(t.Context(), `UPDATE meta SET value=$1 WHERE key='schema_version'`, version)
			require.NoError(t, err)
			require.NoError(t, target.Close())
			//nolint:gosec // Read only the owned test replacement target.
			before, err := os.ReadFile(targetPath)
			require.NoError(t, err)
			preparedPath := filepath.Join(home, "prepared.db")
			prepared := openKataTestDB(t, preparedPath)
			require.NoError(t, prepared.Close())
			require.Error(t, installPreparedSQLiteImport(t.Context(), preparedPath, targetPath, true), "unknown schema with native tables must fail closed")
			//nolint:gosec // Read only the owned test replacement target.
			after, err := os.ReadFile(targetPath)
			require.NoError(t, err)
			require.Equal(t, before, after, "inspection must not upgrade or modify the existing target")
		})
	}
}

func TestImportForceRefusesPreCronAndUnknownTargets(t *testing.T) {
	for _, kind := range []string{"legacy", "unknown", "orphan sidecar"} {
		t.Run(kind, func(t *testing.T) {
			_, input, path := setupImportTest(t)
			marker := path
			if kind == "legacy" {
				target := openKataTestDB(t, path)
				for _, table := range []string{"cron_runs", "cron_jobs", "cron_workflows"} {
					_, err := target.ExecContext(t.Context(), "DROP TABLE "+table)
					require.NoError(t, err)
				}
				_, err := target.ExecContext(t.Context(), `UPDATE meta SET value='30' WHERE key='schema_version'`)
				require.NoError(t, err)
				require.NoError(t, target.Close())
			} else {
				if kind == "orphan sidecar" {
					marker += "-wal"
				}
				require.NoError(t, os.WriteFile(marker, []byte("unrecognized existing state"), 0600))
			}
			//nolint:gosec // Marker is an owned temporary import fixture.
			before, err := os.ReadFile(marker)
			require.NoError(t, err)
			_, err = runCmdOutput(t, nil, "import", "--force", "--input", input, "--target", path)
			require.Error(t, err)
			//nolint:gosec // Marker is the same owned temporary import fixture.
			after, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, before, after)
			if kind == "orphan sidecar" {
				_, err = os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestImportForceCreatesMissingTarget(t *testing.T) {
	_, input, path := setupImportTest(t)
	_, err := runCmdOutput(t, nil, "import", "--force", "--input", input, "--target", path)
	require.NoError(t, err)
	restored := openKataTestDB(t, path)
	defer func() { _ = restored.Close() }()
	_, err = restored.ProjectByName(t.Context(), "kata")
	require.NoError(t, err)
}

func TestInstallPreparedSQLiteImportPreservesMissingCreationTime(t *testing.T) {
	home := setupKataEnv(t)
	path := filepath.Join(home, "target.db")
	target := openKataTestDB(t, path)
	identity := target.InstanceUID()
	_, err := target.ExecContext(t.Context(), `INSERT INTO meta(key,value) VALUES('instance_created_at','2026-10-03T00:00:00Z') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	require.NoError(t, err)
	require.NoError(t, target.Close())
	preparedPath := filepath.Join(home, "prepared.db")
	prepared := openKataTestDB(t, preparedPath)
	_, err = prepared.ExecContext(t.Context(), `UPDATE meta SET value=$1 WHERE key='instance_uid'`, identity)
	require.NoError(t, err)
	_, err = prepared.ExecContext(t.Context(), `DELETE FROM meta WHERE key='instance_created_at'`)
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	require.NoError(t, installPreparedSQLiteImport(t.Context(), preparedPath, path, true))
	restored := openKataTestDB(t, path)
	defer func() { _ = restored.Close() }()
	var count int
	require.NoError(t, restored.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM meta WHERE key='instance_created_at'`).Scan(&count))
	require.Zero(t, count, "install must not reintroduce the displaced target's metadata")
}

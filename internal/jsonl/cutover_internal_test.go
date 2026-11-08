package jsonl

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kit/atomicfile"
)

func TestRewriteV7EventPayloadsUsesDeterministicJSON(t *testing.T) {
	lookup := map[issueLookupKey]issueLookupInfo{
		{projectID: 7, number: 1}: {shortID: "one", uid: "uid-one"},
		{projectID: 7, number: 2}: {shortID: "two", uid: "uid-two"},
	}
	want := `{"payload":{"from_short_id":"one","from_uid":"uid-one","to_short_id":"two","to_uid":"uid-two","type":"related","z":1},"project_id":7,"type":"issue.linked","z":1}`

	for range 100 {
		envs := []Envelope{{Kind: KindEvent, Data: jsontext.Value(`{"z":1,"project_id":7,"type":"issue.linked","payload":{"z":1,"from_number":1,"to_number":2,"type":"related"}}`)}}
		require.NoError(t, rewriteV7EventPayloads(envs, lookup))
		require.Equal(t, want, string(envs[0].Data))
	}
}

func TestImportCutoverTargetRejectsMissingExportEvent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "History to preserve", Author: "tester",
	})
	require.NoError(t, err)
	require.NoError(t, source.Close())

	exportPath := path + ".jsonl"
	expectedEvents, err := exportCutoverSource(ctx, path, exportPath)
	require.NoError(t, err)
	contents, err := os.ReadFile(exportPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	records, err := NewDecoder(bytes.NewReader(contents)).ReadAll(ctx)
	require.NoError(t, err)
	var incomplete bytes.Buffer
	encoder := NewEncoder(&incomplete)
	var dropped bool
	for _, record := range records {
		if record.Kind == KindEvent && !dropped {
			dropped = true
			continue
		}
		require.NoError(t, encoder.Write(record))
	}
	require.True(t, dropped)
	require.NoError(t, os.WriteFile(exportPath, incomplete.Bytes(), 0o600))

	err = importCutoverTarget(ctx, exportPath, path+".target", expectedEvents)

	require.ErrorContains(t, err, "event count mismatch")
}

func TestAutoCutoverRefusesSourceChangedAfterExport(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "exported title", Author: "example-actor",
	})
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Checkpoint(ctx))
	require.NoError(t, source.DB.Close())
	err = autoCutover(ctx, path, cutoverOperations{replaceFile: atomicfile.Replace, exportSource: func(ctx context.Context, sourcePath, exportPath string) (int64, error) {
		count, err := exportCutoverSource(ctx, sourcePath, exportPath)
		if err != nil {
			return count, err
		}
		writer, err := sql.Open("sqlite", sourcePath)
		require.NoError(t, err)
		_, err = writer.ExecContext(ctx, `UPDATE issues SET title='committed after export' WHERE uid=?`, issue.UID)
		require.NoError(t, err)
		// Closing the last writer checkpoints/removes its WAL. A sidecar-only
		// check cannot detect that the exported snapshot is now stale.
		require.NoError(t, writer.Close())
		return count, nil
	}})
	require.ErrorContains(t, err, "source changed during cutover")
	retained, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, retained.Close()) })
	var title, version string
	require.NoError(t, retained.QueryRowContext(ctx, `SELECT title FROM issues WHERE uid=?`, issue.UID).Scan(&title))
	require.Equal(t, "committed after export", title)
	require.NoError(t, retained.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&version))
	require.Equal(t, "28", version)
	backups, err := filepath.Glob(path + ".bak.*")
	require.NoError(t, err)
	require.Empty(t, backups)
	temps, err := filepath.Glob(path + ".import.tmp.*")
	require.NoError(t, err)
	require.Empty(t, temps)
}

func TestAutoCutoverPreservesArtifactsWhenRestoreFails(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Checkpoint(ctx))
	require.NoError(t, source.DB.Close())
	installErr := errors.New("injected install failure")
	restoreErr := errors.New("injected restore failure")
	var backup string
	err = autoCutover(ctx, path, cutoverOperations{
		exportSource: exportCutoverSource,
		replaceFile: func(from, to string) error {
			if from == path {
				backup = to
				return atomicfile.Replace(from, to)
			}
			if from == backup {
				return restoreErr
			}
			return installErr
		},
	})
	require.ErrorIs(t, err, installErr)
	require.ErrorIs(t, err, restoreErr)
	require.NoFileExists(t, path)
	require.FileExists(t, backup)
	require.FileExists(t, path+".import.tmp.jsonl")
	require.FileExists(t, path+".import.tmp.db")
	require.ErrorIs(t, CheckCutoverSource(ctx, path), ErrCutoverInProgress)
}

// Index-only upgrades, and the one that adds the MCP Events table, must
// match the fresh-store catalog without exporting.
func TestAutoCutoverAddsIndexOnlyVersionsInPlace(t *testing.T) {
	for _, version := range []int{41, 44, 48} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			path := indexOnlySource(t, version)
			fresh, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fresh.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, fresh.Close()) })
			var exported bool
			err = autoCutover(t.Context(), path, cutoverOperations{
				replaceFile: atomicfile.Replace,
				exportSource: func(ctx context.Context, sourcePath, exportPath string) (int64, error) {
					exported = true
					return exportCutoverSource(ctx, sourcePath, exportPath)
				},
			})
			require.NoError(t, err)
			require.False(t, exported)
			upgraded, err := sqlitestore.Open(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			got, err := upgraded.SchemaVersion(t.Context())
			require.NoError(t, err)
			require.Equal(t, db.CurrentSchemaVersion(), got)
			catalog := func(store *sqlitestore.Store) [][4]sql.NullString {
				rows, err := store.QueryContext(t.Context(), `SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY name`)
				require.NoError(t, err)
				defer func() { require.NoError(t, rows.Close()) }()
				var entries [][4]sql.NullString
				for rows.Next() {
					var entry [4]sql.NullString
					require.NoError(t, rows.Scan(&entry[0], &entry[1], &entry[2], &entry[3]))
					entries = append(entries, entry)
				}
				require.NoError(t, rows.Err())
				return entries
			}
			require.Equal(t, catalog(fresh), catalog(upgraded))
		})
	}
}

func TestAutoCutoverInPlaceFailureLeavesSourceRetryable(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	path := indexOnlySource(t, 41)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	// Inject a conflicting index to fail the last step after earlier DDL ran.
	_, err = raw.ExecContext(t.Context(), `CREATE INDEX idx_events_moved_from_id ON events(id)`)
	require.NoError(t, err)
	ops := cutoverOperations{exportSource: func(context.Context, string, string) (int64, error) {
		t.Fatal("index-only upgrade exported the source")
		return 0, nil
	}, replaceFile: atomicfile.Replace}
	require.ErrorContains(t, autoCutover(t.Context(), path, ops), "add index idx_events_moved_from_id")
	var version, indexes int
	require.NoError(t, raw.QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key='schema_version'`).Scan(&version))
	require.Equal(t, 41, version)
	require.NoError(t, raw.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name='idx_issues_project_created'`).Scan(&indexes))
	require.Zero(t, indexes)
	_, err = raw.ExecContext(t.Context(), `DROP INDEX idx_events_moved_from_id`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	require.NoError(t, autoCutover(t.Context(), path, ops))
	version, err = sqlitestore.PeekSchemaVersion(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
}

func indexOnlySource(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kata.db")
	source, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.DB.Close() })
	for _, step := range []struct {
		to      int
		indexes []string
		tables  []string
	}{
		{42, []string{"idx_issues_project_created", "idx_issues_created"}, nil},
		{43, []string{"idx_events_resolved_issue_uid"}, nil},
		{44, []string{"idx_events_reset_from"}, nil},
		{45, []string{"idx_issues_uid_prefix", "idx_issues_project_uid_prefix"}, nil},
		{46, []string{"idx_issues_project_ready_priority", "idx_issues_ready_priority"}, nil},
		{47, []string{"idx_issues_ready_updated"}, nil},
		{48, []string{"idx_events_moved_from_id"}, nil},
		{49, []string{"idx_events_cron_run_uid"}, nil},
		{50, nil, []string{"mcp_event_subscriptions"}},
	} {
		if step.to > version {
			for _, name := range step.indexes {
				_, err = source.ExecContext(t.Context(), `DROP INDEX `+name)
				require.NoError(t, err)
			}
			for _, name := range step.tables {
				_, err = source.ExecContext(t.Context(), `DROP TABLE `+name)
				require.NoError(t, err)
			}
		}
	}
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key='schema_version'`, version)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	return path
}

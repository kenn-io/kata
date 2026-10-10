package jsonl_test

import (
	"context"
	"encoding/json/jsontext"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestNativeCronCutoverPreservesVersion30DueRequest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	source := openCutoverTargetDB(t, ctx, path)
	freshShape := sqliteCronPhysicalShape(t, source)
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	owner := "worker"
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Scheduled work", Author: "worker", Owner: &owner, Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-10-01T09:00"`)}})
	require.NoError(t, err)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	due, err := source.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{IssueID: issue.ID, Now: now, DefaultTimezone: "UTC"})
	require.NoError(t, err)
	require.True(t, due.Changed)
	issue, err = source.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	for _, table := range []string{"cron_runs", "cron_jobs", "cron_workflows"} {
		_, err = source.ExecContext(ctx, "DROP TABLE "+table)
		require.NoError(t, err)
	}
	_, err = source.ExecContext(ctx, `UPDATE meta SET value='30' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	target, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	got, err := target.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue, got)
	again, err := target.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{IssueID: issue.ID, Now: now, DefaultTimezone: "UTC"})
	require.NoError(t, err)
	require.False(t, again.Changed)
	version, err := target.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 32, version)
	require.Equal(t, freshShape, sqliteCronPhysicalShape(t, target))
	rows, err := target.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Empty(t, rows)
}

func sqliteCronPhysicalShape(t *testing.T, store *sqlitestore.Store) []string {
	t.Helper()
	rows, err := store.QueryContext(t.Context(), `SELECT type || ':' || name || ':' || COALESCE(sql,'') FROM sqlite_master WHERE name GLOB 'cron_*' OR tbl_name GLOB 'cron_*' ORDER BY type,name`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var shape []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		shape = append(shape, value)
	}
	require.NoError(t, rows.Err())
	return shape
}

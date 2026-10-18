package db_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	_ "modernc.org/sqlite"
)

func TestRecurringDeliveryNonObjectMetadata(t *testing.T) {
	// Only these test-local tables omit the production metadata-object constraint.
	query, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, query.Close()) })
	query.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE federation_bindings(project_id INTEGER,role TEXT,enabled INTEGER)`,
		`CREATE TABLE issues(id INTEGER,uid TEXT,project_id INTEGER,status TEXT,owner TEXT,author TEXT,metadata TEXT,revision INTEGER,deleted_at TEXT)`,
	} {
		_, err = query.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
	anchor := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	job := db.CronJob{CronDefinition: db.CronDefinition{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW"}, Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", Title: "Review service"}, Overlap: "allow", Catchup: "latest"}}
	other := job
	other.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	policy := *job.Definition.Issue
	policy.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
	other.Definition.Issue = &policy
	metadata, err := json.Marshal(map[string]any{"cron.progress." + other.UID: cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}})
	require.NoError(t, err)
	for i, row := range []struct{ uid, metadata string }{{job.Definition.Issue.ParentUID, "[]"}, {other.Definition.Issue.ParentUID, string(metadata)}} {
		_, err = query.ExecContext(t.Context(), `INSERT INTO issues VALUES($1,$2,1,'open',NULL,'operator',$3,1,NULL)`, i+1, row.uid, row.metadata)
		require.NoError(t, err)
	}
	store := db.CronSQL{Query: query, RecurringChildren: func(context.Context, int64, string) ([]db.RecurringChild, error) { return nil, nil }}
	out, err := store.RecurringDelivery(t.Context(), 1, []db.CronJob{job, other}, anchor.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "error", out[0].State)
	require.NotEmpty(t, out[0].Error)
	require.Equal(t, "active", out[1].State)
	require.Empty(t, out[1].Error)
	// A recipient error does not erase a successfully evaluated pending batch.
	metadata, err = json.Marshal(map[string]any{"cron.progress." + job.UID: cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}})
	require.NoError(t, err)
	_, err = query.ExecContext(t.Context(), `UPDATE issues SET metadata=$1,author=$2 WHERE id=1`, string(metadata), "invalid\nrecipient")
	require.NoError(t, err)
	out, err = store.RecurringDelivery(t.Context(), 1, []db.CronJob{job, other}, anchor.Add(5*time.Hour))
	require.NoError(t, err)
	require.Equal(t, "error", out[0].State)
	require.NotEmpty(t, out[0].Error)
	require.Nil(t, out[0].NextDueAt)
	require.NotNil(t, out[0].Pending)
	require.Equal(t, 1, out[0].Pending.Count)

}

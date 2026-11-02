package db

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Missing reply targets must use an indexed probe rather than scan event history.
func TestPurgedReplyTargetEvidenceUsesReplyIndex(t *testing.T) {
	pool, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	schema, err := os.ReadFile("sqlitestore/schema.sql")
	require.NoError(t, err)
	_, err = pool.ExecContext(t.Context(), string(schema))
	require.NoError(t, err)
	rows, err := pool.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+commentGraphPurgedTargetEvidenceSQL(func(int) string { return "?" }, `json_extract(e.payload,'$.reply_to_uid')`), "missing")
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "USING INDEX idx_events_reply_to", "plan: %s", joined)
	require.NotContains(t, joined, "SCAN e", "plan: %s", joined)
}

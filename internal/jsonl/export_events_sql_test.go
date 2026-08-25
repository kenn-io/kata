package jsonl

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

var updateEventSQLGolden = flag.Bool("update-golden", false,
	"rewrite the event export SQL goldens from the current code")

// errSQLCaptured stops each export function right after it has assembled its
// query, so the golden test needs no database.
var errSQLCaptured = errors.New("sql captured")

type sqlCapture struct {
	queries []string
	args    [][]any
}

func (c *sqlCapture) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	c.queries = append(c.queries, query)
	c.args = append(c.args, args)
	return nil, errSQLCaptured
}

func (c *sqlCapture) QueryRowContext(_ context.Context, query string, _ ...any) *sql.Row {
	panic("event export must not use QueryRowContext: " + query)
}

// TestEventExportSQLIsPinnedPerVersionBand locks the exact query each event
// projection emits. The kata#1 peer-scrub rule and the subject-liveness
// filter are pure string construction spread across five version bands, and
// the emitted SQL must stay byte-identical per band: a changed projection
// silently alters cutover output for one source-version band only, and event
// ordering feeds federated short_id assignment.
//
// Regenerate with `-update-golden` ONLY when a query is deliberately changed,
// and review the resulting diff as the specification of that change.
func TestEventExportSQLIsPinnedPerVersionBand(t *testing.T) {
	cases := []struct {
		name     string
		version  int
		opts     ExportOptions
		wantArgs []any
	}{
		{"v1_live_only", 1, ExportOptions{ProjectID: 7}, []any{int64(7)}},
		{"v1_include_deleted", 1, ExportOptions{ProjectID: 7, IncludeDeleted: true}, []any{int64(7)}},
		{"v2_live_only", 2, ExportOptions{ProjectID: 7}, []any{int64(7)}},
		{"v2_include_deleted", 2, ExportOptions{ProjectID: 7, IncludeDeleted: true}, []any{int64(7)}},
		{"v3_live_only", 3, ExportOptions{ProjectID: 7}, []any{int64(7)}},
		{"v3_include_deleted", 3, ExportOptions{ProjectID: 7, IncludeDeleted: true}, []any{int64(7)}},
		{"v8_live_only", 8, ExportOptions{ProjectID: 7}, []any{int64(7)}},
		{"v8_include_deleted", 8, ExportOptions{ProjectID: 7, IncludeDeleted: true}, []any{int64(7)}},
		{"current_live_only", db.CurrentSchemaVersion(), ExportOptions{ProjectID: 7}, []any{int64(7)}},
		{"current_include_deleted", db.CurrentSchemaVersion(), ExportOptions{ProjectID: 7, IncludeDeleted: true}, []any{int64(7)}},
		{"current_all_projects_live_only", db.CurrentSchemaVersion(), ExportOptions{}, []any{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &sqlCapture{}
			err := exportEvents(context.Background(), capture, NewEncoder(io.Discard), tc.opts, tc.version)
			require.ErrorIs(t, err, errSQLCaptured)
			require.Len(t, capture.queries, 1, "each band emits exactly one events query")
			assert.Equal(t, tc.wantArgs, capture.args[0])
			assertGoldenSQL(t, tc.name, capture.queries[0])
		})
	}
}

// TestEventExportV1OmitsRelatedIssueUID guards the one deliberate divergence
// between the bands: the v1 events table has no related_issue_uid column, so
// the shared scrub rule must not emit that expression there.
func TestEventExportV1OmitsRelatedIssueUID(t *testing.T) {
	for _, opts := range []ExportOptions{{}, {IncludeDeleted: true}} {
		capture := &sqlCapture{}
		err := exportEvents(context.Background(), capture, NewEncoder(io.Discard), opts, 1)
		require.ErrorIs(t, err, errSQLCaptured)
		assert.NotContains(t, capture.queries[0], "related_issue_uid")
	}
}

func assertGoldenSQL(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "events_sql", name+".sql")
	if *updateEventSQLGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o600))
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // testdata path built from a fixed case name
	require.NoError(t, err, "missing golden %s; regenerate with -update-golden and review the diff", path)
	assert.Equal(t, string(want), got)
}

package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kit/search/sqlquery"
)

func TestReadFederationQuietPageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	project := createProject(ctx, t, d, "quiet-project")
	_, err := d.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	issue, _ := createTesterIssue(ctx, t, d, project.ID, "Quiet issue")
	busy := createProject(ctx, t, d, "busy-project")
	after, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),?,?,'busy-project','project.metadata_updated','tester','{}',1,0,? FROM n`, d.InstanceUID(), busy.ID, strings.Repeat("a", 64))
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	store := &sqlitestore.Store{DB: raw}
	_, err = d.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES('00000000000000000000999999',$1,$2,'quiet-project',$3,'sync.reset_required','tester','{}',1,0,$4)`, d.InstanceUID(), project.ID, issue.UID, strings.Repeat("a", 64))
	require.NoError(t, err)

	for _, analyzed := range []bool{false, true} {
		if analyzed {
			_, err = raw.ExecContext(ctx, "ANALYZE")
			require.NoError(t, err)
		}
		for _, subject := range []string{"", issue.UID} {
			t.Run(fmt.Sprintf("analyzed=%t/issue=%t", analyzed, subject != ""), func(t *testing.T) {
				conn, err := raw.Conn(ctx)
				require.NoError(t, err)
				sqlitePageFetches(t, conn, true)
				require.NoError(t, conn.Close())
				result, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, IssueUID: subject, AfterID: after, Limit: 100, Features: db.SupportedEventFeatures})
				require.NoError(t, err)
				require.Empty(t, result.Events)
				require.Equal(t, after, result.NextAfterID)
				conn, err = raw.Conn(ctx)
				require.NoError(t, err)
				pages := sqlitePageFetches(t, conn, false)
				require.NoError(t, conn.Close())
				t.Logf("%d page fetches for an unchanged poll", pages)
				require.LessOrEqual(t, pages, 256, "quiet polls must skip unrelated history")
			})
		}
	}
}

func TestReadFederationBulkMovePageBudget(t *testing.T) {
	for _, interleaved := range []bool{false, true} {
		t.Run(fmt.Sprintf("interleaved=%t", interleaved), func(t *testing.T) {
			store, path := openTestDBWithPath(t)
			project, after := dbtest.SeedFederatedReadBulkHistory(t, store, interleaved)
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			raw.SetMaxOpenConns(1)
			conn, err := raw.Conn(t.Context())
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			tx, err := raw.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			events, err := db.ReadFederationEventPage(t.Context(), tx, project, "", after, 1, false, func(where string, args ...any) ([]db.Event, error) {
				return (sqlquery.Query{SQL: sqlitestore.EventSelectForTest + where, Args: args}).All(t.Context(), tx, func(rows *sql.Rows) (db.Event, error) { return sqlitestore.ScanEventForTest(rows) })
			})
			require.NoError(t, err)
			require.NoError(t, tx.Rollback())
			require.Len(t, events, 1)
			stride := int64(1)
			if interleaved {
				stride = 2
			}
			require.Equal(t, after+stride, events[0].ID)
			conn, err = raw.Conn(t.Context())
			require.NoError(t, err)
			pages := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("%d page fetches", pages)
			require.LessOrEqual(t, pages, 256, "bulk histories must cost a small multiple of the page size")
		})
	}
}

func TestReadFederationSparseCatchupPageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	project := createProject(ctx, t, d, "catchup-project")
	_, err := d.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	busy := createProject(ctx, t, d, "busy-project")
	foreign, _ := createTesterIssue(ctx, t, d, busy.ID, "Foreign issue")
	// Keep the issue count above the event gap, so each window finds a few
	// events and a full page requires many windows, with no empty-window seek.
	const issueCount, gap, limit = 2000, 300, 300
	_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO issues(uid,short_id,project_id,title,author)
 SELECT printf('%026d',x),printf('%06d',x),$2,'Catchup issue','tester' FROM n`, issueCount, project.ID)
	require.NoError(t, err)
	after, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),$2,CASE WHEN x%$3=0 THEN $4 ELSE $5 END,
 CASE WHEN x%$3=0 THEN 'catchup-project' ELSE 'busy-project' END,
 CASE WHEN x%$3=0 THEN printf('%026d',1) ELSE $7 END,'issue.updated','tester','{}',1,0,$6 FROM n`, limit*(gap+1), d.InstanceUID(), gap+1, project.ID, busy.ID, strings.Repeat("a", 64), foreign.UID)
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	tx, err := raw.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	windows := 0
	query := func(where string, args ...any) ([]db.Event, error) {
		windows++
		return (sqlquery.Query{SQL: sqlitestore.EventSelectForTest + where, Args: args}).All(ctx, tx, func(rows *sql.Rows) (db.Event, error) { return sqlitestore.ScanEventForTest(rows) })
	}
	events, err := db.ReadFederationEventPage(ctx, tx, project, "", after, limit, false, query)
	require.NoError(t, err)
	require.Len(t, events, limit)
	for i, event := range events {
		require.Equal(t, after+int64((i+1)*(gap+1)), event.ID)
	}
	// A constant issue-count-wide window needs 46 queries for this history.
	const constantWidthWindows = (limit*(gap+1) + issueCount - 1) / issueCount
	t.Logf("%d window queries; constant width needs %d", windows, constantWidthWindows)
	require.LessOrEqual(t, windows, constantWidthWindows/4, "sparse catchup must widen productive windows")
}

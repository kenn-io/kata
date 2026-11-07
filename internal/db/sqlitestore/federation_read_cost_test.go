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

	for _, populated := range []bool{false, true} {
		want := []int64{}
		nextAfter := after
		if populated {
			title := "Occasional update"
			_, event, _, err := d.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "tester"})
			require.NoError(t, err)
			want = append(want, event.ID)
			nextAfter = event.ID
		}

		for _, analyzed := range []bool{false, true} {
			if analyzed {
				_, err = raw.ExecContext(ctx, "ANALYZE")
				require.NoError(t, err)
			}
			for _, subject := range []string{"", issue.UID} {
				t.Run(fmt.Sprintf("populated=%t/analyzed=%t/issue=%t", populated, analyzed, subject != ""), func(t *testing.T) {
					conn, err := raw.Conn(ctx)
					require.NoError(t, err)
					sqlitePageFetches(t, conn, true)
					require.NoError(t, conn.Close())
					for range 2 {
						result, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, IssueUID: subject, AfterID: after, Limit: 100, Features: db.SupportedEventFeatures})
						require.NoError(t, err)
						require.Len(t, result.Events, len(want))
						if populated {
							require.Equal(t, want[0], result.Events[0].ID)
						}
						require.Equal(t, nextAfter, result.NextAfterID)
					}
					conn, err = raw.Conn(ctx)
					require.NoError(t, err)
					pages := sqlitePageFetches(t, conn, false)
					require.NoError(t, conn.Close())
					t.Logf("%d page fetches for two unchanged polls", pages)
					require.LessOrEqual(t, pages, 512, "quiet polls must skip unrelated history")
				})
			}
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
			for _, analyzed := range []bool{false, true} {
				if analyzed {
					_, err = raw.ExecContext(t.Context(), "ANALYZE")
					require.NoError(t, err)
				}
				conn, err := raw.Conn(t.Context())
				require.NoError(t, err)
				sqlitePageFetches(t, conn, true)
				require.NoError(t, conn.Close())
				tx, err := raw.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				events, err := db.ReadFederationEventPage(t.Context(), tx, project, "", after, 1000, false, func(where string, args ...any) ([]db.Event, error) {
					return (sqlquery.Query{SQL: sqlitestore.EventSelectForTest + where, Args: args}).All(t.Context(), tx, func(rows *sql.Rows) (db.Event, error) { return sqlitestore.ScanEventForTest(rows) })
				})
				require.NoError(t, err)
				require.NoError(t, tx.Rollback())
				require.Len(t, events, 1000)
				stride := int64(1)
				if interleaved {
					stride = 2
				}
				for i, e := range events {
					require.Equal(t, after+int64(i+1)*stride, e.ID)
				}
				conn, err = raw.Conn(t.Context())
				require.NoError(t, err)
				pages := sqlitePageFetches(t, conn, false)
				require.NoError(t, conn.Close())
				t.Logf("analyzed=%t: %d page fetches", analyzed, pages)
				require.LessOrEqual(t, pages, 3000, "bulk histories must cost a small multiple of the page size")
			}
		})
	}
}

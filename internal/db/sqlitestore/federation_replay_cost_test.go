package sqlitestore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"modernc.org/sqlite"
)

func TestFederationReplayCostUnchangedIssues(t *testing.T) {
	var small int
	for _, count := range []int{8, 64} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			d, p, _ := incrementalTestStore(t)
			ctx := t.Context()
			for range count {
				issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "work", Author: "tester"})
				require.NoError(t, err)
				_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Body: "note", Author: "tester"})
				require.NoError(t, err)
			}
			_, err := d.DB.ExecContext(ctx, `UPDATE federation_bindings SET role = 'spoke', hub_url = 'https://hub.example', hub_project_id = ?, hub_project_uid = ? WHERE project_id = ?`, p.ID, p.UID, p.ID)
			require.NoError(t, err)
			var path string
			var seq int
			var name string
			require.NoError(t, d.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &path))
			raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			raw.SetMaxOpenConns(1)
			store := &Store{DB: raw}
			require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
			pages := func(reset bool) int {
				conn, err := raw.Conn(ctx)
				require.NoError(t, err)
				defer func() { require.NoError(t, conn.Close()) }()
				total := 0
				require.NoError(t, conn.Raw(func(raw any) error {
					for _, op := range []sqlite.DBStatusOp{sqlite.DBStatusCacheHit, sqlite.DBStatusCacheMiss} {
						n, _, err := raw.(sqlite.DBStatus).Status(op, reset)
						if err != nil {
							return err
						}
						total += n
					}
					return nil
				}))
				return total
			}
			pages(true)
			require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
			fetches := pages(false)
			t.Logf("%d unchanged issues: %d page fetches", count+1, fetches)
			if count == 8 {
				small = fetches
			} else {
				// Full replay still scans history and joins comments to issues.
				// Leave room for those reads, below the per-row allocation/write cost.
				require.LessOrEqual(t, fetches, small+384)
			}
		})
	}
}

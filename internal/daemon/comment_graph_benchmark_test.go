package daemon

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func BenchmarkShowIssueCommentGraph(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("comments=%d", count), func(b *testing.B) {
			ctx := b.Context()
			store, err := sqlitestore.Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(b, err)
			selected, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Selected", Author: "worker"})
			require.NoError(b, err)
			for range 5 {
				_, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: selected.ID, Author: "worker", Body: "Selected comment"})
				require.NoError(b, err)
			}
			for _, trigger := range []string{"comments_ai_fts", "comments_au_fts", "comments_ad_fts"} {
				_, err := store.ExecContext(ctx, "DROP TRIGGER "+trigger) // #nosec G202 -- Fixed benchmark trigger names.
				require.NoError(b, err)
			}
			tx, err := store.BeginTx(ctx, nil)
			require.NoError(b, err)
			defer func() { _ = tx.Rollback() }()
			for index := range 1000 {
				_, err := tx.ExecContext(ctx, `INSERT INTO issues(id,uid,short_id,project_id,title,author) VALUES(?,?,?,?,?,?)`, 10000+index, fmt.Sprintf("01M4%022d", index), fmt.Sprintf("%05d", index), project.ID, "Other", "worker")
				require.NoError(b, err)
			}
			for index := range count - 5 {
				_, err := tx.ExecContext(ctx, `INSERT INTO comments(uid,issue_id,author,body) VALUES(?,?,?,?)`, fmt.Sprintf("01M5%022d", index), 10000+index%1000, "worker", "Unrelated comment")
				require.NoError(b, err)
			}
			require.NoError(b, tx.Commit())
			config := ServerConfig{DB: store, InsecureReadonly: true}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_, err := hydrateShowIssueResponse(ctx, config, selected, false)
				require.NoError(b, err)
			}
		})
	}
}

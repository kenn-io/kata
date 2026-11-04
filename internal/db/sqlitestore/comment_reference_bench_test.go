package sqlitestore

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func BenchmarkReadCommentGraphFullUIDReference(b *testing.B) {
	for _, comments := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("comments=%d", comments), func(b *testing.B) {
			ctx := b.Context()
			d, err := Open(ctx, filepath.Join(b.TempDir(), "kata.db"))
			require.NoError(b, err)
			b.Cleanup(func() { _ = d.Close() })
			// Dropping the FTS triggers speeds up setup without affecting the read path.
			for _, statement := range []string{
				`DROP TRIGGER comments_ai_fts`,
				`DROP TRIGGER comments_au_fts`,
				`DROP TRIGGER comments_ad_fts`,
				`INSERT INTO projects(id,uid,name) VALUES(2,'01M40000000000000000000001','example-project')`,
				`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000) INSERT INTO issues(id,uid,short_id,project_id,title,author) SELECT x,printf('01M4%022d',x),printf('%04d',x),2,'Issue','worker' FROM n`,
			} {
				_, err := d.ExecContext(ctx, statement)
				require.NoError(b, err)
			}
			_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO comments(uid,issue_id,author,body) SELECT printf('01M5%022d',x),1+(x-1)%1000,'worker','Comment' FROM n`, comments)
			require.NoError(b, err)
			query := db.CommentGraphQuery{ProjectID: 2, Reference: &db.CommentGraphReference{UID: fmt.Sprintf("01M5%022d", comments)}}
			b.ResetTimer()
			for b.Loop() {
				_, err := d.ReadCommentGraph(ctx, query)
				require.NoError(b, err)
			}
		})
	}
}

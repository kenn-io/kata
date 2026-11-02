package sqlitestore

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Missing reply targets must use an indexed probe rather than scan event history.
func TestPurgedReplyTargetEvidenceUsesReplyIndex(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	rows, err := store.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+db.CommentGraphPurgedTargetEvidenceSQL(func(int) string { return "?" }, commentReplyTargetUIDExpr), "missing")
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

func BenchmarkReadCommentGraphPendingReplyTargets(b *testing.B) {
	for _, history := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("events=%d", history), func(b *testing.B) {
			ctx := b.Context()
			store, err := Open(ctx, filepath.Join(b.TempDir(), "kata.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(b, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "Replies"})
			require.NoError(b, err)
			for range 3 {
				targetUID, err := uid.New()
				require.NoError(b, err)
				_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Pending reply", ReplyToUID: targetUID, ReplyKind: "reply"})
				require.NoError(b, err)
			}
			tx, err := store.BeginTx(ctx, nil)
			require.NoError(b, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000)
INSERT INTO purge_log(uid,origin_instance_uid,issue_uid,project_id,purged_issue_id,project_name,issue_title,issue_author,comment_count,link_count,label_count,event_count,actor)
SELECT printf('%026d',x),?,printf('%026d',x),?,x,'example-project','Purged','worker',0,0,0,0,'worker' FROM n`, store.InstanceUID(), project.ID)
			require.NoError(b, err)
			_, err = tx.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,related_issue_uid)
SELECT printf('%026d',x),?,?,'example-project',?,'issue.commented','worker','{"reply_to_uid":"unrelated-target"}',1,0,printf('%064d',x),printf('%026d',1+(x%1000)) FROM n`, history, store.InstanceUID(), project.ID, issue.UID)
			require.NoError(b, err)
			require.NoError(b, tx.Commit())
			query := db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: issue.ID}
			b.ResetTimer()
			for range b.N {
				graph, err := store.ReadCommentGraph(ctx, query)
				require.NoError(b, err)
				require.Len(b, graph.Comments, 3)
			}
		})
	}
}
